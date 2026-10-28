package work

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

type RemoteProofCommand func(context.Context, string, ...string) *exec.Cmd

func validateRemoteProofRoute(r RemoteProofRoute) error {
	if r.SchemaVersion != "compound.remote_go_test_route.v0.1" || r.Host == "" || strings.HasPrefix(r.Host, "-") || strings.ContainsAny(r.Host, " \t\r\n'\\\"") {
		return fmt.Errorf("invalid fixed remote host")
	}
	if !filepath.IsAbs(r.RunnerPath) || strings.ContainsAny(r.RunnerPath, "\x00\r\n") || !remoteHex(r.RunnerSHA256, 64) {
		return fmt.Errorf("exact absolute remote program and digest required")
	}
	if !regexp.MustCompile(`^go[0-9]+\.[0-9]+(\.[0-9]+)?$`).MatchString(r.GoVersion) || strings.ContainsAny(r.GoVersion, " \t\r\n") || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 7200 {
		return fmt.Errorf("invalid remote toolchain or deadline")
	}
	config, err := base64.StdEncoding.DecodeString(r.ConfigBase64)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("exact remote config required: %w", err)
	}
	root, ok := cfg["VERIFY_ROOT"].(string)
	if !ok || !filepath.IsAbs(root) || root == "/" {
		return fmt.Errorf("remote config requires explicit owned VERIFY_ROOT")
	}
	if cfg["GOTOOLCHAIN_PIN"] != r.GoVersion {
		return fmt.Errorf("remote config toolchain must equal exact descriptor")
	}
	_, err = remoteCanonicalJSON(r)
	return err
}
func SealRemoteProofProfile(ctx context.Context, profile ProofResourceProfile, routePath string, command RemoteProofCommand) (ProofResourceProfile, error) {
	if profile.SourceSnapshot == nil {
		return profile, fmt.Errorf("remote proof requires exact source snapshot")
	}
	body, err := os.ReadFile(routePath)
	if err != nil {
		return profile, err
	}
	if len(body) > 1<<20 {
		return profile, fmt.Errorf("remote route exceeds limit")
	}
	var route RemoteProofRoute
	if err := remoteStrictJSON(body, &route); err != nil {
		return profile, err
	}
	if err := validateRemoteProofRoute(route); err != nil {
		return profile, err
	}
	output, err := command(ctx, "go", "env", "GOVERSION").Output()
	if err != nil {
		return profile, err
	}
	if strings.TrimSpace(string(output)) != route.GoVersion {
		return profile, fmt.Errorf("local and remote Go versions differ")
	}
	roots := append([]string{}, ProofCeremonyOwnedIgnoredInputs...)
	slices.Sort(roots)
	entries, _, err := remoteProofManifest(ctx, profile.Repo, roots, command)
	if err != nil {
		return profile, err
	}
	d := RemoteProofDescriptor{Route: route, Source: *profile.SourceSnapshot, InputRoots: roots, Inputs: entries}
	d.InputsDigest, err = remoteInputsDigest(roots, entries)
	if err != nil {
		return profile, err
	}
	d.GateDigest, err = remoteGateDigest(d.Source.HeadSHA)
	if err != nil {
		return profile, err
	}
	d.Digest, err = remoteDigest(d)
	if err != nil {
		return profile, err
	}
	profile.RemoteGoTest = &d
	return normalizeProofResourceProfile(profile)
}
func remoteProofManifest(ctx context.Context, repo string, roots []string, command RemoteProofCommand) ([]RemoteProofInput, map[string]string, error) {
	if err := validateRemoteProofRoots(roots); err != nil {
		return nil, nil, err
	}
	entries := []RemoteProofInput{}
	locations := map[string]string{}
	main := proofMainCheckout(ctx, repo, command)
	for _, root := range roots {
		source := ""
		for _, base := range []string{repo, main} {
			if base == "" {
				continue
			}
			candidate := filepath.Join(base, filepath.FromSlash(root))
			if _, err := os.Lstat(candidate); err == nil {
				source = candidate
				break
			} else if !os.IsNotExist(err) {
				return nil, nil, err
			}
		}
		if source == "" {
			entries = append(entries, RemoteProofInput{Path: root, Kind: "absent"})
			continue
		}
		rootInfo, err := os.Lstat(source)
		if err != nil {
			return nil, nil, err
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("remote input root cannot be symlink")
		}
		resolvedRoot, err := filepath.EvalSymlinks(source)
		if err != nil {
			return nil, nil, err
		}
		locations[root] = source
		err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			name := root
			if rel != "." {
				name += "/" + filepath.ToSlash(rel)
			}
			if strings.ContainsAny(name, "\\\x00\u2028\u2029") {
				return fmt.Errorf("unsupported remote input path")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			item := RemoteProofInput{Path: name}
			switch {
			case info.IsDir():
				item.Kind = "directory"
			case info.Mode().IsRegular():
				item.Kind = "file"
				if info.Size() > 4<<30 {
					return fmt.Errorf("remote input exceeds per-file bound")
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				h := sha256.New()
				_, copyErr := copyProofContext(ctx, h, f)
				closeErr := f.Close()
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
				item.SHA256 = hex.EncodeToString(h.Sum(nil))
				executable := info.Mode()&0111 != 0
				item.Executable = &executable
			case info.Mode()&os.ModeSymlink != 0:
				if path == source {
					return fmt.Errorf("remote input root cannot be symlink")
				}
				item.Kind = "symlink"
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				if filepath.IsAbs(target) || strings.ContainsAny(target, "\\\x00\u2028\u2029") {
					return fmt.Errorf("unsupported remote input symlink")
				}
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(resolvedRoot, resolved)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return fmt.Errorf("remote input symlink escapes root")
				}
				item.Target = target
			default:
				return fmt.Errorf("unsupported remote input file type")
			}
			if len(entries) >= 200000 {
				return fmt.Errorf("remote input manifest exceeds 200000 entries")
			}
			entries = append(entries, item)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	slices.SortFunc(entries, func(a, b RemoteProofInput) int { return strings.Compare(a.Path, b.Path) })
	if err := validateRemoteProofInputs(roots, entries); err != nil {
		return nil, nil, err
	}
	return entries, locations, nil
}

type remoteBoundedWriter struct {
	writer io.Writer
	count  int64
}

func (w *remoteBoundedWriter) Write(b []byte) (int, error) {
	if w.count+int64(len(b)) > 8<<30 {
		return 0, fmt.Errorf("remote archive exceeds 8 GiB")
	}
	n, e := w.writer.Write(b)
	w.count += int64(n)
	return n, e
}
func remoteProofArchive(ctx context.Context, repo string, request RemoteProofRequest, command RemoteProofCommand) (string, func() error, map[string]int64, map[string]int64, error) {
	if err := validateRemoteProofInputs(request.InputRoots, request.Inputs); err != nil {
		return "", nil, nil, nil, err
	}
	temp, err := os.MkdirTemp("", "compound-remote-proof-")
	if err != nil {
		return "", nil, nil, nil, err
	}
	cleanup := func() error { return os.RemoveAll(temp) }
	timings := map[string]int64{}
	counts := map[string]int64{}
	fail := func(e error) (string, func() error, map[string]int64, map[string]int64, error) {
		_ = cleanup()
		return "", nil, nil, nil, e
	}
	started := time.Now()
	for expr, want := range map[string]string{"HEAD": request.HeadSHA, "HEAD^{tree}": request.SourceTree} {
		cmd := command(ctx, "git", "-C", repo, "rev-parse", "--verify", expr)
		out, e := cmd.Output()
		if e != nil || strings.TrimSpace(string(out)) != want {
			return fail(fmt.Errorf("sealed source identity changed: %s", expr))
		}
	}
	bundle := filepath.Join(temp, "source.bundle")
	if out, e := command(ctx, "git", "-C", repo, "bundle", "create", bundle, "HEAD").CombinedOutput(); e != nil {
		return fail(fmt.Errorf("source bundle: %w: %s", e, out))
	}
	timings["bundle_create"] = time.Since(started).Milliseconds()
	started = time.Now()
	entries, locations, err := remoteProofManifest(ctx, repo, request.InputRoots, command)
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(entries, request.Inputs) {
		return fail(fmt.Errorf("sealed ceremony inputs differ from reserved manifest"))
	}
	timings["inputs_hash"] = time.Since(started).Milliseconds()
	archive := filepath.Join(temp, "request.tar")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(err)
	}
	bound := &remoteBoundedWriter{writer: f}
	tw := tar.NewWriter(bound)
	write := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	requestBody, err := remoteCanonicalJSON(request)
	if err == nil && len(requestBody) > 16<<20 {
		err = fmt.Errorf("remote request exceeds bound")
	}
	if err == nil {
		err = write("request.json", requestBody)
	}
	addFile := func(name, path, expected string, executable bool) error {
		src, e := os.Open(path)
		if e != nil {
			return e
		}
		defer src.Close()
		info, e := src.Stat()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<30 {
			return fmt.Errorf("remote file exceeds bound or changed type")
		}
		mode := int64(0600)
		if executable {
			mode = 0700
		}
		if e = tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg}); e != nil {
			return e
		}
		h := sha256.New()
		n, e := copyProofContext(ctx, io.MultiWriter(tw, h), src)
		if e != nil {
			return e
		}
		if n != info.Size() {
			return fmt.Errorf("remote input size changed")
		}
		if expected != "" && hex.EncodeToString(h.Sum(nil)) != expected {
			return fmt.Errorf("remote input changed while copying")
		}
		counts["file_payload"] += n
		return nil
	}
	if err == nil {
		err = addFile("source.bundle", bundle, "", false)
	}
	started = time.Now()
	for _, entry := range entries {
		if err != nil {
			break
		}
		if e := ctx.Err(); e != nil {
			err = e
			break
		}
		name := "inputs/" + entry.Path
		switch entry.Kind {
		case "absent":
			continue
		case "directory":
			err = tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0700, Typeflag: tar.TypeDir})
		case "symlink":
			err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0777, Typeflag: tar.TypeSymlink, Linkname: entry.Target})
		case "file":
			root := ""
			for _, candidate := range request.InputRoots {
				if entry.Path == candidate || strings.HasPrefix(entry.Path, candidate+"/") {
					root = candidate
					break
				}
			}
			relative := strings.TrimPrefix(strings.TrimPrefix(entry.Path, root), "/")
			err = addFile(name, filepath.Join(locations[root], filepath.FromSlash(relative)), entry.SHA256, entry.Executable != nil && *entry.Executable)
		}
	}
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fail(err)
	}
	timings["inputs_copy_archive"] = time.Since(started).Milliseconds()
	counts["archive"] = bound.count
	return archive, cleanup, timings, counts, nil
}
