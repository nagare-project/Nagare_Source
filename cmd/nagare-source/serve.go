package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/pluginapi"
	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
	"github.com/nagare-project/Nagare_Source/internal/webresolver"
)

func serve(arguments []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root containing schema/ and sources/")
	listenAddress := flags.String("listen", "127.0.0.1:7788", "loopback listen address")
	version := flags.String("version", buildVersion, "plugin version reported by manifest and health")
	chrome := flags.String("chrome", "", "optional Chrome/Chromium executable path")
	installedDir := flags.String("installed-dir", os.Getenv(installedSourcesEnv),
		"optional directory of user-installed source documents (default $"+installedSourcesEnv+")")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if err := validateListenAddress(*listenAddress); err != nil {
		return err
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	absoluteInstalled := ""
	if *installedDir != "" {
		if absoluteInstalled, err = filepath.Abs(*installedDir); err != nil {
			return err
		}
	}
	handler, err := newPluginHandler(absoluteRoot, *version, *chrome, absoluteInstalled)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return err
	}
	defer listener.Close()
	if address, ok := listener.Addr().(*net.TCPAddr); !ok || !address.IP.IsLoopback() {
		return errors.New("Plugin API listener must resolve to a loopback address")
	}
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 * 1024,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return shutdownContext
		},
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()
	if err := writePluginReady(os.Stdout, listener.Addr().String()); err != nil {
		return err
	}
	select {
	case <-shutdownContext.Done():
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(deadline); err != nil {
			return err
		}
		err := <-serverErrors
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type pluginReadyEvent struct {
	Event    string `json:"event"`
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
}

// writePluginReady 是 serve 写入 stdout 的唯一条记录。Nagare 读取这条
// 有界 JSON 来发现系统分配的回环端口，诊断信息仍保留在 stderr。
func writePluginReady(output io.Writer, address string) error {
	return json.NewEncoder(output).Encode(pluginReadyEvent{
		Event: "ready", Protocol: "nagare-plugin-launch/v1", URL: "http://" + address,
	})
}

// buildVersion 由发布流水线以 -ldflags "-X main.buildVersion=<tag>" 注入；
// 本地构建保持 dev。--version 显式传入时仍以传入值为准。
var buildVersion = "dev"

// installedSourcesEnv 指向宿主（Nagare）为用户保存“从地址安装”的来源目录。
// 用环境变量而不是新增必填参数：旧宿主不设置它，插件行为不变。
const installedSourcesEnv = "NAGARE_SOURCE_INSTALLED_DIR"

// pluginCapabilities 中的 installed_sources 表示插件提供 POST /v1/sources/inspect，
// 并在启动时加载 installed-dir 中的独立来源。
var pluginCapabilities = []string{"web", "browser_sniff", "bt", "bt_releases", "ndjson", "installed_sources"}

func newPluginHandler(root, version, chromePath, installedDir string) (*pluginapi.Handler, error) {
	loaded, err := loadRunners(root, chromePath, installedDir)
	if err != nil {
		return nil, err
	}
	return pluginapi.New(pluginapi.Options{
		Root: root,
		Manifest: pluginapi.Manifest{
			ID: "org.nagare.source.community", Name: "Nagare Community Sources", Version: version,
			ProtocolVersions: []int{1}, SourceSchemaVersions: []int{1},
			Capabilities: append([]string(nil), pluginCapabilities...),
		},
		Runners:   loaded.runners,
		Installed: loaded.installed,
		Logf:      log.Printf,
	})
}

func loadSourceRunners(root, chromePath string) ([]sourceruntime.Runner, error) {
	loaded, err := loadRunners(root, chromePath, "")
	if err != nil {
		return nil, err
	}
	return loaded.runners, nil
}

type loadedRunners struct {
	runners   []sourceruntime.Runner
	installed map[string]bool
}

// loadRunners 加载仓库来源，再叠加 installedDir 中用户独立安装的来源。
// 同 id 时以用户安装的版本为准（这是用户的显式选择）；独立来源不使用仓库 fixture。
// 坏的独立来源只记日志跳过，仓库来源仍按原规则任何错误即失败。
func loadRunners(root, chromePath, installedDir string) (loadedRunners, error) {
	validator, err := repository.NewValidator(root)
	if err != nil {
		return loadedRunners{}, err
	}
	sources, err := repository.LoadSources(root, validator)
	if err != nil {
		return loadedRunners{}, err
	}
	installed := map[string]bool{}
	if installedDir != "" {
		extra, problems := repository.LoadInstalledSources(installedDir, validator)
		for _, problem := range problems {
			log.Printf("skip installed source %s", problem.Error())
		}
		replaced := map[string]bool{}
		for _, source := range extra {
			id, _ := source.Document["id"].(string)
			installed[id] = true
			replaced[id] = true
		}
		merged := make([]repository.SourceDocument, 0, len(sources)+len(extra))
		for _, source := range sources {
			if id, _ := source.Document["id"].(string); !replaced[id] {
				merged = append(merged, source)
			}
		}
		sources = append(merged, extra...)
	}
	var browserBackend webresolver.Browser = webresolver.NewChrome(webresolver.ChromeOptions{ExecutablePath: chromePath})
	if helperPath := webresolver.FindNativeWebViewHelper(root); os.Getenv("NAGARE_SOURCE_NATIVE_WEBVIEW") == "1" && helperPath != "" {
		browserBackend = webresolver.NewNativeWebView(webresolver.NativeWebViewOptions{ExecutablePath: helperPath, MaxSessions: 2})
	}
	browser := webresolver.New(browserBackend)
	// 浏览器解析日志默认关闭；排障时显式打开，输出已按 RedactURL 脱敏（无 query/凭据）。
	if os.Getenv("NAGARE_SOURCE_DEBUG") == "1" {
		browser.Logger = webresolver.LoggerFunc(log.Printf)
	}
	runners := make([]sourceruntime.Runner, 0, len(sources))
	for _, source := range sources {
		options := sourceruntime.RunnerOptions{Browser: browser}
		if id, _ := source.Document["id"].(string); !installed[id] {
			options.FixtureFetcher = sourceFixtureFetcher(root, source.Document)
		}
		runners = append(runners, sourceruntime.NewSpecRunner(source.Document, options))
	}
	return loadedRunners{runners: runners, installed: installed}, nil
}

func sourceFixtureFetcher(root string, source map[string]any) btcrawler.Fetcher {
	if kind, _ := source["kind"].(string); kind != "bt" {
		return nil
	}
	sourceID, _ := source["id"].(string)
	for _, extension := range []string{".xml", ".rss", ".json", ".txt"} {
		path := filepath.Join(root, "fixtures", "responses", sourceID+extension)
		if data, err := os.ReadFile(path); err == nil {
			return btcrawler.StaticFetcher{Response: btcrawler.Response{Body: data}}
		}
	}
	return nil
}

func validateListenAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("--listen must use an explicit loopback IP such as 127.0.0.1 or ::1")
	}
	return nil
}
