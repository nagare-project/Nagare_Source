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
	version := flags.String("version", "dev", "plugin version reported by manifest and health")
	chrome := flags.String("chrome", "", "optional Chrome/Chromium executable path")
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
	handler, err := newPluginHandler(absoluteRoot, *version, *chrome)
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

func newPluginHandler(root, version, chromePath string) (*pluginapi.Handler, error) {
	runners, err := loadSourceRunners(root, chromePath)
	if err != nil {
		return nil, err
	}
	return pluginapi.New(pluginapi.Options{
		Root: root,
		Manifest: pluginapi.Manifest{
			ID: "org.nagare.source.community", Name: "Nagare Community Sources", Version: version,
			ProtocolVersions: []int{1}, SourceSchemaVersions: []int{1},
			Capabilities: []string{"web", "browser_sniff", "bt", "ndjson"},
		},
		Runners: runners,
	})
}

func loadSourceRunners(root, chromePath string) ([]sourceruntime.Runner, error) {
	validator, err := repository.NewValidator(root)
	if err != nil {
		return nil, err
	}
	sources, err := repository.LoadSources(root, validator)
	if err != nil {
		return nil, err
	}
	browser := webresolver.New(webresolver.NewChrome(webresolver.ChromeOptions{ExecutablePath: chromePath}))
	runners := make([]sourceruntime.Runner, 0, len(sources))
	for _, source := range sources {
		runners = append(runners, sourceruntime.NewSpecRunner(source.Document, sourceruntime.RunnerOptions{
			Browser: browser, FixtureFetcher: sourceFixtureFetcher(root, source.Document),
		}))
	}
	return runners, nil
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
