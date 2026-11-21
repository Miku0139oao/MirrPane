// Package ffmpeg finds or installs the shared FFmpeg runtime. It never needs
// headers, import libraries, a C compiler, or a system-wide installation.
package ffmpeg

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// DLLs must come from one compatible Windows x64 build. The corresponding
// structure layouts are checked into internal/decode/abi_windows_amd64.go.
var DLLs = [...]string{"avutil-61.dll", "swresample-7.dll", "swscale-10.dll", "avcodec-63.dll"}

const (
	releaseURL = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/tags/latest"
	assetName  = "ffmpeg-master-latest-win64-gpl-shared.zip"
	maxArchive = 512 << 20
)

func complete(dir string) bool {
	for _, name := range DLLs {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func cached(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "current"))
	name := strings.TrimSpace(string(data))
	if err != nil || name == "." || !strings.HasPrefix(name, "runtime-") || filepath.Base(name) != name {
		return ""
	}
	dir := filepath.Join(root, name)
	if complete(dir) {
		return dir
	}
	return ""
}

// Find prefers an explicit override, then the portable application, the local
// development tree and finally PATH. An override is authoritative: a typo must
// not silently load a different installation.
func Find() (string, error) {
	if dir := os.Getenv("MIRRPANE_FFMPEG_DIR"); dir != "" {
		dir, err := filepath.Abs(dir)
		if err != nil || !complete(dir) {
			return "", fmt.Errorf("FFmpeg: MIRRPANE_FFMPEG_DIR does not contain the required shared DLLs: %s", dir)
		}
		return dir, nil
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		dirs = append(dirs, dir, filepath.Join(dir, "ffmpeg"), cached(filepath.Join(dir, "ffmpeg")))
	}
	if cwd, err := os.Getwd(); err == nil {
		for range 6 {
			root := filepath.Join(cwd, "third_party", "ffmpeg")
			dirs = append(dirs, filepath.Join(root, "bin"), cached(root))
			cwd = filepath.Dir(cwd)
		}
	}
	if exe, err := exec.LookPath("ffmpeg.exe"); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	for _, dir := range dirs {
		if dir != "" && complete(dir) {
			return filepath.Abs(dir)
		}
	}
	return "", errors.New("FFmpeg: compatible shared runtime not found")
}

// Ensure reuses an installed runtime, or downloads the latest upstream shared
// build into a portable cache. Existing installations are never auto-upgraded
// while in use. A newer incompatible ABI is rejected before loading any DLL.
// An optional validator checks exports/ABI before selecting a runtime, allowing
// automatic discovery to recover from damaged or incompatible local libraries.
func Ensure(ctx context.Context, root string, progress func(string), validate func(string) error) (string, error) {
	check := func(dir string) error {
		if validate != nil {
			return validate(dir)
		}
		return nil
	}
	if dir, err := Find(); err == nil {
		if err = check(dir); err == nil {
			return dir, nil
		} else if os.Getenv("MIRRPANE_FFMPEG_DIR") != "" {
			return "", err
		}
		// Prefer an already downloaded replacement for an incompatible or
		// damaged local installation, including on subsequent offline starts.
		if cache := cached(root); cache != "" && cache != dir {
			if err := check(cache); err == nil {
				return cache, nil
			}
		}
	} else if os.Getenv("MIRRPANE_FFMPEG_DIR") != "" {
		return "", err
	}
	dir, err := (installer{client: &http.Client{Timeout: 10 * time.Minute}, releaseURL: releaseURL}).install(ctx, root, progress)
	if err == nil {
		err = check(dir)
	}
	return dir, err
}

// DownloadLatest is also used by the development/packaging helper so CI and
// first-run installation use identical verification and extraction rules.
func DownloadLatest(ctx context.Context, root string, progress func(string)) (string, error) {
	return (installer{client: &http.Client{Timeout: 10 * time.Minute}, releaseURL: releaseURL}).install(ctx, root, progress)
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type installer struct {
	client     *http.Client
	releaseURL string
}

func (i installer) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MirrPane-FFmpeg-Setup")
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("FFmpeg download: HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	return resp, nil
}

func (i installer) install(ctx context.Context, root string, progress func(string)) (string, error) {
	report := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	report("正在查詢最新版 FFmpeg…")
	resp, err := i.get(ctx, i.releaseURL)
	if err != nil {
		return "", err
	}
	var release struct {
		Assets []releaseAsset `json:"assets"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&release)
	resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("FFmpeg release metadata: %w", err)
	}
	var asset releaseAsset
	for _, candidate := range release.Assets {
		if candidate.Name == assetName {
			asset = candidate
			break
		}
	}
	digest := strings.TrimPrefix(asset.Digest, "sha256:")
	expected, err := hex.DecodeString(digest)
	if err != nil || len(expected) != sha256.Size || !strings.HasPrefix(asset.Digest, "sha256:") || asset.URL == "" || asset.Size <= 0 || asset.Size > maxArchive {
		return "", errors.New("FFmpeg release is missing a valid Windows x64 shared asset or SHA256 digest")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("FFmpeg cache: %w", err)
	}
	name := "runtime-" + digest[:16]
	destination := filepath.Join(root, name)
	if !complete(destination) {
		stage, err := os.MkdirTemp(root, ".download-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(stage)
		report("正在下載 FFmpeg（首次啟動可能需要幾分鐘）…")
		archivePath := filepath.Join(stage, "runtime.zip")
		if err := i.download(ctx, asset, archivePath); err != nil {
			return "", err
		}
		report("正在驗證與安裝 FFmpeg…")
		dllDir := filepath.Join(stage, "bin")
		if err := extract(archivePath, dllDir); err != nil {
			return "", err
		}
		// Publish only a complete, verified runtime. A concurrent instance may
		// have installed the same content while this download was in flight.
		if err := os.Rename(dllDir, destination); err != nil && !complete(destination) {
			return "", fmt.Errorf("FFmpeg install: %w", err)
		}
	}
	marker, err := os.CreateTemp(root, ".current-")
	if err != nil {
		return "", err
	}
	defer os.Remove(marker.Name())
	_, writeErr := marker.WriteString(name + "\n")
	closeErr := marker.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	if err := os.Rename(marker.Name(), filepath.Join(root, "current")); err != nil {
		return "", err
	}
	report("FFmpeg 已就緒")
	return filepath.Abs(destination)
}

func (i installer) download(ctx context.Context, asset releaseAsset, destination string) error {
	resp, err := i.get(ctx, asset.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(destination)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, asset.Size+1))
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if n != asset.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != asset.Digest {
		return errors.New("FFmpeg download failed size/SHA256 verification")
	}
	return nil
}

func extract(archivePath, destination string) error {
	z, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer z.Close()
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	files := make(map[string]bool)
	var total uint64
	for _, entry := range z.File {
		name := path.Base(entry.Name)
		wanted := false
		for _, dll := range DLLs {
			wanted = wanted || name == dll
		}
		if !wanted {
			continue
		}
		total += entry.UncompressedSize64
		if files[name] || !entry.Mode().IsRegular() || total > maxArchive || entry.UncompressedSize64 > 256<<20 {
			return errors.New("FFmpeg archive contains duplicate, invalid or oversized DLLs")
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		// Only known basenames are written; archive paths are never extracted.
		f, err := os.OpenFile(filepath.Join(destination, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			source.Close()
			return err
		}
		n, copyErr := io.Copy(f, io.LimitReader(source, 256<<20+1))
		err = errors.Join(copyErr, f.Close(), source.Close())
		if err != nil || n == 0 || uint64(n) != entry.UncompressedSize64 {
			return fmt.Errorf("FFmpeg archive: invalid %s: %w", name, errors.Join(err, io.ErrUnexpectedEOF))
		}
		files[name] = true
	}
	if !complete(destination) {
		return fmt.Errorf("latest FFmpeg shared build has an unsupported ABI; update MirrPane (requires %s)", strings.Join(DLLs[:], ", "))
	}
	return nil
}
