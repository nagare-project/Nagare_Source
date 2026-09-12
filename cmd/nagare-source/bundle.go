package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// bundle 生成可随播放器安装包分发的【运行时根目录】：只含某一类来源（当前只有 bt），
// 加上校验它们所需的 schema、fixture 响应、空的上游批准表，再为这个子集重新生成
// 健康报告并做一次完整校验。
//
// 为什么只带 BT：磁力是 infohash 指针，与在线直链的法律性质不同；播放器安装包
// 只捆 BT 规则，在线规则仍由用户主动给仓库地址（Nagare 侧红线 3 的约束①按此改写）。
func bundle(arguments []string) error {
	flags := flag.NewFlagSet("bundle", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	output := flags.String("out", "", "output directory for the runtime root (must not exist or be empty)")
	profile := flags.String("profile", "bt", "which sources to include: bt")
	generatedAt := flags.String("generated-at", "", "RFC 3339 health report time; defaults to current UTC time")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--out is required")
	}
	if *profile != "bt" {
		return fmt.Errorf("unsupported bundle profile %q; only bt is supported", *profile)
	}
	if entries, err := os.ReadDir(*output); err == nil && len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty", *output)
	}

	sourceIDs, err := copyBundleSources(*root, *output, *profile)
	if err != nil {
		return err
	}
	if err := copyTree(filepath.Join(*root, "schema"), filepath.Join(*output, "schema"), func(string) bool { return true }); err != nil {
		return fmt.Errorf("copy schema: %w", err)
	}
	// 只带这些来源自己的 fixture 响应：selfcheck fixture 模式靠它，不许带上游站点的原始响应。
	responses := filepath.Join(*root, "fixtures", "responses")
	if err := copyTree(responses, filepath.Join(*output, "fixtures", "responses"), func(name string) bool {
		base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
		return sourceIDs[base]
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("copy fixtures: %w", err)
	}
	// 捆绑包不含任何导入的上游规则，批准表为空但必须存在（validate 会读它）。
	if err := os.MkdirAll(filepath.Join(*output, "upstreams"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*output, "upstreams", "approved.json"), []byte("{\n  \"schema\": \"nagare-upstream-approvals/v1\",\n  \"upstreams\": []\n}\n"), 0o644); err != nil {
		return err
	}

	healthArguments := []string{"--root", *output, "--mode", "fixture",
		"--out", filepath.Join(*output, "reports", "health.json"), "--html", ""}
	if *generatedAt != "" {
		healthArguments = append(healthArguments, "--generated-at", *generatedAt)
	}
	if err := health(healthArguments); err != nil {
		return fmt.Errorf("health report for bundle: %w", err)
	}
	if err := validate([]string{"--root", *output}); err != nil {
		return fmt.Errorf("bundle validation: %w", err)
	}
	fmt.Printf("bundled %d %s sources into %s\n", len(sourceIDs), *profile, *output)
	return nil
}

// copyBundleSources 复制 sources/<profile>/ 下的规则文件，返回其中的来源 id 集合。
func copyBundleSources(root, output, profile string) (map[string]bool, error) {
	from := filepath.Join(root, "sources", profile)
	to := filepath.Join(output, "sources", profile)
	ids := map[string]bool{}
	err := copyTree(from, to, func(name string) bool {
		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".yml" {
			return false
		}
		ids[strings.TrimSuffix(filepath.Base(name), ext)] = true
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("copy %s sources: %w", profile, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no %s sources found under %s", profile, from)
	}
	return ids, nil
}

// copyTree 递归复制普通文件（跳过符号链接），keep 决定哪些文件进包。
func copyTree(from, to string, keep func(path string) bool) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if entry.Type()&fs.ModeSymlink != 0 || !keep(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
