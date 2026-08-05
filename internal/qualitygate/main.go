// Command qualitygate runs starter-smtp's repository-owned cross-platform checks.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	requiredGoVersion = "go1.26.5"
	modulePath        = "github.com/spice-framework/starter-smtp"
	minimumCoverage   = 85.0
)

var output = log.New(os.Stdout, "", 0)

func main() {
	os.Exit(execute()) // Entrypoint exception: propagate verification failure.
}

func execute() int {
	mode := flag.String("mode", "verify", "verification mode: check, fmt, or verify")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	root, err := repositoryRoot()
	if err == nil {
		err = run(ctx, root, *mode)
	}
	if err != nil {
		output.Printf("quality gate failed: %v", err)
		return 1
	}
	return 0
}

type step struct {
	name string
	run  func() error
}

func run(ctx context.Context, root, mode string) error {
	if runtime.Version() != requiredGoVersion {
		return fmt.Errorf("go version is %s; require exactly %s", runtime.Version(), requiredGoVersion)
	}
	identity := step{"repository identity", func() error { return checkIdentity(root) }}
	formatting := step{"formatting", func() error { return format(ctx, root, false) }}
	modules := step{"module and vendor", func() error { return checkModule(ctx, root) }}
	vet := step{"go vet", func() error { return command(ctx, root, nil, "go", "vet", "./...") }}
	var steps []step
	switch mode {
	case "check":
		steps = []step{identity, formatting, modules, vet}
	case "fmt":
		steps = []step{{"formatting", func() error { return format(ctx, root, true) }}}
	case "verify":
		steps = []step{
			identity,
			formatting,
			modules,
			vet,
			{"lint and nil safety", func() error { return lint(ctx, root) }},
			{"security", func() error { return security(ctx, root) }},
			{"shuffled and race tests", func() error { return tests(ctx, root) }},
			{"coverage", func() error { return coverage(ctx, root) }},
			{"offline vendor", func() error { return offline(ctx, root) }},
		}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	for _, current := range steps {
		started := time.Now()
		output.Printf("==> %s", current.name)
		if err := current.run(); err != nil {
			return fmt.Errorf("%s (%s): %w", current.name, time.Since(started).Round(time.Millisecond), err)
		}
		output.Printf("<== %s passed in %s", current.name, time.Since(started).Round(time.Millisecond))
	}
	output.Print("==> all verification passed")
	return nil
}

func checkIdentity(root string) error {
	content, err := os.ReadFile(filepath.Join(root, "go.mod")) // #nosec G304 -- root is resolved from this repository's module identity.
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	if !strings.Contains(string(content), "module "+modulePath+"\n") {
		return fmt.Errorf("go.mod does not declare canonical module %s", modulePath)
	}
	if bytes.Contains(content, []byte("\nreplace ")) || bytes.Contains(content, []byte("\nreplace (")) {
		return errors.New("committed go.mod must not contain replace directives")
	}
	return nil
}

func format(ctx context.Context, root string, write bool) error {
	files, err := goFiles(root)
	if err != nil {
		return err
	}
	for _, name := range []string{"goimports", "gofumpt"} {
		executable, pathErr := toolPath(ctx, root, name)
		if pathErr != nil {
			return pathErr
		}
		option := "-l"
		if write {
			option = "-w"
		}
		stdout, runErr := capture(ctx, root, nil, executable, append([]string{option}, files...)...)
		if runErr != nil {
			return runErr
		}
		if !write && strings.TrimSpace(stdout) != "" {
			return fmt.Errorf("%s requires formatting: %s", name, strings.Join(strings.Fields(stdout), ", "))
		}
	}
	return nil
}

func goFiles(root string) ([]string, error) {
	var result []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root && slices.Contains([]string{".git", "tools", "vendor"}, entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" {
			result = append(result, path)
		}
		return nil
	})
	slices.Sort(result)
	return result, err
}

func checkModule(ctx context.Context, root string) error {
	if err := command(ctx, root, nil, "go", "mod", "tidy", "-diff"); err != nil {
		return err
	}
	if err := command(ctx, root, nil, "go", "-C", "tools", "mod", "tidy", "-diff"); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "spice-starter-smtp-vendor-")
	if err != nil {
		return fmt.Errorf("create vendor comparison directory: %w", err)
	}
	defer removeTree(temporary)
	candidate := filepath.Join(temporary, "vendor")
	if vendorErr := command(ctx, root, nil, "go", "mod", "vendor", "-o", candidate); vendorErr != nil {
		return vendorErr
	}
	current, err := treeDigests(filepath.Join(root, "vendor"))
	if err != nil {
		return err
	}
	expected, err := treeDigests(candidate)
	if err != nil {
		return err
	}
	if !maps.Equal(current, expected) {
		return errors.New("vendor differs from a fresh go mod vendor result")
	}
	return nil
}

func removeTree(path string) {
	if err := os.RemoveAll(path); err != nil {
		output.Printf("warning: remove temporary tree %q: %v", path, err)
	}
}

func treeDigests(root string) (map[string][sha256.Size]byte, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open vendor root: %w", err)
	}
	defer func() {
		if closeErr := opened.Close(); closeErr != nil {
			output.Printf("warning: close vendor root %q: %v", root, closeErr)
		}
	}()
	result := make(map[string][sha256.Size]byte)
	err = fs.WalkDir(opened.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, readErr := opened.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		result[filepath.ToSlash(path)] = sha256.Sum256(content)
		return nil
	})
	return result, err
}

func lint(ctx context.Context, root string) error {
	golangci, err := toolPath(ctx, root, "golangci-lint")
	if err != nil {
		return err
	}
	if lintErr := command(ctx, root, nil, golangci, "run", "--timeout=10m"); lintErr != nil {
		return lintErr
	}
	nilaway, err := toolPath(ctx, root, "nilaway")
	if err != nil {
		return err
	}
	return command(ctx, root, nil, nilaway, "-include-pkgs="+modulePath, "./...")
}

func security(ctx context.Context, root string) error {
	gosec, err := toolPath(ctx, root, "gosec")
	if err != nil {
		return err
	}
	if gosecErr := command(ctx, root, nil, gosec, "-quiet", "-exclude-generated", "./..."); gosecErr != nil {
		return gosecErr
	}
	govulncheck, err := toolPath(ctx, root, "govulncheck")
	if err != nil {
		return err
	}
	return command(ctx, root, nil, govulncheck, "./...")
}

func tests(ctx context.Context, root string) error {
	if err := command(ctx, root, nil, "go", "test", "-shuffle=on", "-count=1", "./..."); err != nil {
		return err
	}
	return command(ctx, root, nil, "go", "test", "-race", "-shuffle=on", "-count=1", "./...")
}

func coverage(ctx context.Context, root string) (returnErr error) {
	profile, err := os.CreateTemp("", "spice-starter-smtp-coverage-*.out")
	if err != nil {
		return fmt.Errorf("create coverage profile: %w", err)
	}
	path := profile.Name()
	if closeErr := profile.Close(); closeErr != nil {
		return fmt.Errorf("close coverage profile: %w", closeErr)
	}
	defer func() { returnErr = errors.Join(returnErr, os.Remove(path)) }()
	if coverageErr := command(ctx, root, nil, "go", "test", "-covermode=atomic", "-coverprofile="+path, "."); coverageErr != nil {
		return coverageErr
	}
	stdout, err := capture(ctx, root, nil, "go", "tool", "cover", "-func="+path)
	if err != nil {
		return err
	}
	percentage, err := totalCoverage(stdout)
	if err != nil {
		return err
	}
	output.Printf("coverage %.1f%% (minimum %.1f%%)", percentage, minimumCoverage)
	if percentage < minimumCoverage {
		return fmt.Errorf("coverage %.1f%% is below %.1f%%", percentage, minimumCoverage)
	}
	return nil
}

func totalCoverage(report string) (float64, error) {
	lines := strings.Split(strings.TrimSpace(report), "\n")
	if len(lines) == 0 {
		return 0, errors.New("coverage report is empty")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 || !strings.HasSuffix(fields[len(fields)-1], "%") {
		return 0, errors.New("coverage report has no total percentage")
	}
	value := strings.TrimSuffix(fields[len(fields)-1], "%")
	percentage, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parse coverage percentage %q: %w", value, err)
	}
	return percentage, nil
}

func offline(ctx context.Context, root string) error {
	environment := map[string]string{"GOFLAGS": "-mod=vendor"}
	if err := command(ctx, root, environment, "go", "test", "-count=1", "./..."); err != nil {
		return err
	}
	return command(ctx, root, environment, "go", "build", "-trimpath", "./...")
}

func toolPath(ctx context.Context, root, name string) (string, error) {
	stdout, err := capture(ctx, root, nil, "go", "tool", "-C", "tools", "-n", name)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(stdout)
	if path == "" {
		return "", fmt.Errorf("resolve tool %q: empty path", name)
	}
	return path, nil
}

func repositoryRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		content, readErr := os.ReadFile(filepath.Join(current, "go.mod")) // #nosec G304 -- candidates are bounded ancestors.
		if readErr == nil && bytes.Contains(content, []byte("module "+modulePath)) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("find starter-smtp repository root: go.mod not found")
		}
		current = parent
	}
}

func command(ctx context.Context, directory string, environment map[string]string, executable string, arguments ...string) error {
	// #nosec G204,G702 -- executable and arguments are fixed repository-owned values.
	cmd := exec.CommandContext(ctx, executable, arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(environment)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", executable, strings.Join(arguments, " "), err)
	}
	return nil
}

func capture(ctx context.Context, directory string, environment map[string]string, executable string, arguments ...string) (string, error) {
	// #nosec G204,G702 -- executable and arguments are fixed repository-owned values.
	cmd := exec.CommandContext(ctx, executable, arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(environment)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", executable, strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func mergedEnvironment(overrides map[string]string) []string {
	values := map[string]string{"GOWORK": "off", "GOPROXY": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local"}
	maps.Copy(values, overrides)
	result := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := values[strings.ToUpper(key)]; !replaced {
				result = append(result, entry)
			}
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result
}
