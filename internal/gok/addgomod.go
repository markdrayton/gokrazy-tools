package gok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"

	gokversion "github.com/gokrazy/tools/internal/version"
	"golang.org/x/mod/module"
	"golang.org/x/sync/errgroup"
)

type resolvedModule struct {
	module  string
	version string
	goMod   []byte
}

type latestResp struct {
	Version string `json:"Version"`
}

func proxyRequest(proxyBase, importPath, suffix string) (*http.Request, error) {
	escapedSuffix, err := module.EscapeVersion(suffix)
	if err != nil {
		return nil, err
	}
	if escapedSuffix != "@latest" {
		escapedSuffix = "@v/" + escapedSuffix
	}
	escapedImportPath, err := module.EscapePath(importPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", proxyBase+"/"+escapedImportPath+"/"+escapedSuffix, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gokrazy gok "+gokversion.ReadBrief())
	return req, nil
}

func moduleInfo(ctx context.Context, proxyBase, importPath, version string) (*latestResp, error) {
	suffix := version + ".info"
	if version == "latest" {
		suffix = "@latest"
	}
	req, err := proxyRequest(proxyBase, importPath, suffix)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return nil, fmt.Errorf("unexpected HTTP status: got %v, want %v", resp.Status, want)
	}
	var latest latestResp
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP response: %v", err)
	}
	if err := json.Unmarshal(b, &latest); err != nil {
		return nil, fmt.Errorf("decoding /@latest response: %v", err)
	}
	return &latest, nil
}

func resolveGoMod(ctx context.Context, proxyBase, importPath string, latest *latestResp) (*resolvedModule, error) {
	req, err := proxyRequest(proxyBase, importPath, latest.Version+".mod")
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return nil, fmt.Errorf("unexpected HTTP status: got %v, want %v", resp.Status, want)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP response: %v", err)
	}
	return &resolvedModule{
		module:  importPath,
		version: latest.Version,
		goMod:   b,
	}, nil
}

// resolveModule finds the Go module containing importPath at version.
//
// For public modules, it queries the module proxy directly over HTTP (fast,
// parallel). When the module proxy must not or cannot be used — importPath
// matches GOPRIVATE/GONOPROXY, GOPROXY does not start with a proxy URL, or the
// proxy does not know the module — it falls back to the go command, which
// honors the user's full Go configuration (including settings made with
// 'go env -w') and git credentials.
func resolveModule(ctx context.Context, importPath, version string) (*resolvedModule, error) {
	env, err := goEnv(ctx, "GOPROXY", "GONOPROXY")
	if err != nil {
		return nil, err
	}
	proxyBase, ok := firstProxyURL(env["GOPROXY"])
	if !ok || module.MatchPrefixPatterns(env["GONOPROXY"], importPath) {
		return resolveModuleGo(ctx, importPath, version)
	}
	resolved, err := resolveModuleProxy(ctx, proxyBase, importPath, version)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		// Not found on the proxy. The go command may still find it, e.g.
		// via a 'direct' fallback in GOPROXY. Its error messages are also
		// more helpful than ours.
		return resolveModuleGo(ctx, importPath, version)
	}
	return resolved, nil
}

// candidateModulePaths returns the prefixes of importPath that could be a
// module path, longest first.
func candidateModulePaths(importPath string) []string {
	parts := strings.Split(path.Clean(importPath), "/")
	var candidates []string
	for idx := len(parts); idx > 0; idx-- {
		candidate := strings.Join(parts[:idx], "/")
		if candidate == "github.com" {
			// Short-circuit: github.com is not a Go module :)
			continue
		}
		if strings.HasPrefix(candidate, "github.com/") &&
			!strings.ContainsRune(strings.TrimPrefix(candidate, "github.com/"), '/') {
			// Short-circuit: github.com/<something> references an
			// organisation or user, not a repository.
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// goEnv returns the effective values of the specified go env variables,
// including those configured via 'go env -w'.
func goEnv(ctx context.Context, keys ...string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "go", append([]string{"env", "-json"}, keys...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %v", cmd.Args, err)
	}
	env := make(map[string]string)
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("decoding go env output: %v", err)
	}
	return env, nil
}

// firstProxyURL returns the first entry of GOPROXY if it is a proxy URL (as
// opposed to "direct" or "off").
func firstProxyURL(goproxy string) (string, bool) {
	first, _, _ := strings.Cut(goproxy, ",")
	first, _, _ = strings.Cut(first, "|")
	first = strings.TrimSpace(first)
	if !strings.HasPrefix(first, "https://") && !strings.HasPrefix(first, "http://") {
		return "", false
	}
	return strings.TrimSuffix(first, "/"), true
}

type goModDownload struct {
	Path    string
	Version string
	GoMod   string
	Error   string
}

// resolveModuleGo resolves importPath using 'go mod download', trying each
// candidate module path, longest first.
func resolveModuleGo(ctx context.Context, importPath, version string) (*resolvedModule, error) {
	tmp, err := os.MkdirTemp("", "gok-add-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	var errs []string
	for _, candidate := range candidateModulePaths(importPath) {
		cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", candidate+"@"+version)
		// Run outside of any module or workspace the user might be in.
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, runErr := cmd.Output()
		var dl goModDownload
		if err := json.Unmarshal(out, &dl); err != nil {
			if runErr != nil {
				errs = append(errs, fmt.Sprintf("%s: %v: %s", candidate, runErr, strings.TrimSpace(stderr.String())))
				continue
			}
			return nil, fmt.Errorf("decoding go mod download output: %v", err)
		}
		if dl.Error != "" {
			errs = append(errs, dl.Error)
			continue
		}
		b, err := os.ReadFile(dl.GoMod)
		if err != nil {
			return nil, err
		}
		return &resolvedModule{
			module:  dl.Path,
			version: dl.Version,
			goMod:   b,
		}, nil
	}
	return nil, fmt.Errorf("could not resolve import path %q to any Go module:\n\t%s", importPath, strings.Join(errs, "\n\t"))
}

// resolveModuleProxy resolves importPath via the module proxy at proxyBase.
// It returns nil (and no error) if the proxy does not know the module.
func resolveModuleProxy(ctx context.Context, proxyBase, importPath, version string) (*resolvedModule, error) {
	candidates := candidateModulePaths(importPath)
	resps := make([]*latestResp, len(candidates))
	eg, latestctx := errgroup.WithContext(ctx)
	for idx, candidate := range candidates {
		eg.Go(func() error {
			resp, err := moduleInfo(latestctx, proxyBase, candidate, version)
			if err != nil {
				return err
			}
			resps[idx] = resp
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// candidates are ordered longest first, so the first hit is the most
	// specific module containing importPath.
	for idx, candidate := range candidates {
		if resp := resps[idx]; resp != nil {
			return resolveGoMod(ctx, proxyBase, candidate, resp)
		}
	}

	return nil, nil
}
