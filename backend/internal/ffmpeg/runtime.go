// Package ffmpeg finds or installs the shared FFmpeg runtime. It never needs
// headers, import libraries, a C compiler, or a system-wide installation.
package ffmpeg

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DLLs must come from one compatible Windows x64 build. The corresponding
// structure layouts are checked into internal/decode/abi_windows_amd64.go.
var DLLs = [...]string{"avutil-61.dll", "swresample-7.dll", "swscale-10.dll", "avcodec-63.dll"}

// Generated from the archive identified by assetSHA256. This also validates
// existing installations created before the downloader wrote local manifests.
//
//go:embed pinned-files.sha256
var pinnedFiles []byte

const (
	// The asset filename omits the date. Packaging cross-checks this dated ABI
	// build ID against assetName and releaseTag. Update with the decode layouts.
	buildID     = "N-129166-gae9f4540cb-20261121"
	releaseTag  = "autobuild-2026-11-21-12-37"
	releaseURL  = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/tags/" + releaseTag
	assetName   = "ffmpeg-N-129166-gae9f4540cb-win64-gpl-shared.zip"
	assetURL    = "https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/" + releaseTag + "/" + assetName
	assetSHA256 = "20b19e93d8597a42f91b73adaf852034403668542311a422e77ff9d323c6b744"
	maxArchive  = 512 << 20

	// manifestName records the SHA-256 of every extracted file. A cache entry
	// without a manifest uses the verified inventory when its name is pinned.
	manifestName = "SHA256SUMS"
)

func knownFile(name string) bool {
	if name == "COPYING" {
		return true
	}
	for _, dll := range DLLs {
		if name == dll {
			return true
		}
	}
	return false
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyFiles reports whether dir holds all required DLLs and, when the
// directory has a manifest, whether every recorded file still matches it.
func verifyFiles(dir string) bool {
	for _, name := range DLLs {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	data := pinnedFiles
	if filepath.Base(filepath.Clean(dir)) != "runtime-"+assetSHA256[:16] {
		var err error
		data, err = os.ReadFile(filepath.Join(dir, manifestName))
		if errors.Is(err, os.ErrNotExist) {
			return true // custom compatible runtime, checked by the ABI loader
		}
		if err != nil {
			return false
		}
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 2*sha256.Size {
			return false
		}
		digest, name := fields[0], fields[1]
		if filepath.Base(name) != name || name == manifestName || seen[name] || !knownFile(name) {
			return false
		}
		seen[name] = true
		actual, err := fileSHA256(filepath.Join(dir, name))
		if err != nil || actual != digest {
			return false
		}
	}
	for _, dll := range DLLs {
		if !seen[dll] {
			return false // a manifest must cover every shipped DLL
		}
	}
	return true
}

func complete(dir string) bool { return verifyFiles(dir) }

// installed additionally requires the licence text that packaging relies on.
func installed(dir string) bool {
	if !verifyFiles(dir) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "COPYING"))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
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

// Find prefers an explicit MIRRPANE_FFMPEG_DIR override, then the directory
// next to the executable and the app's own cache directory beside it. An
// override is authoritative: a typo must not silently load a different
// installation. PATH and the current working directory are never consulted.
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
		dirs = append(dirs, dir, filepath.Join(dir, "ffmpeg"))
		if c := cached(filepath.Join(dir, "ffmpeg")); c != "" {
			dirs = append(dirs, c)
		}
	}
	seen := make(map[string]bool)
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if complete(dir) {
			return filepath.Abs(dir)
		}
	}
	return "", errors.New("FFmpeg: compatible shared runtime not found")
}

// Ensure reuses an installed runtime, or downloads the pinned upstream shared
// build into a portable cache. Existing installations are never auto-upgraded
// while in use. A newer incompatible ABI is rejected before loading any DLL.
// An optional validator checks exports/ABI before selecting a runtime, allowing
// automatic discovery to recover from damaged or incompatible local libraries.
func Ensure(ctx context.Context, root string, progress func(string), validate func(string) error) (string, error) {
	return ensure(ctx, root, progress, validate, pinnedInstaller())
}

func ensure(ctx context.Context, root string, progress func(string), validate func(string) error, ins installer) (string, error) {
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
	} else if cache := cached(root); cache != "" {
		if err := check(cache); err == nil {
			return cache, nil
		}
	}
	dir, err := ins.install(ctx, root, progress)
	if err != nil {
		return "", err
	}
	if err = check(dir); err == nil {
		return dir, nil
	}
	// The installed copy passed the completeness check but failed ABI/export
	// validation. Replace it once with a freshly verified download so a
	// damaged but present cache repairs itself; the same pinned content failing
	// again is reported as the original validation error.
	if repaired, repairErr := ins.installWith(ctx, root, progress, true); repairErr == nil {
		if err = check(repaired); err == nil {
			return repaired, nil
		}
	}
	return "", err
}

// DownloadLatest retains its original public name for the setup helper, but
// downloads only the pinned build. CI and first-run use identical verification.
func DownloadLatest(ctx context.Context, root string, progress func(string)) (string, error) {
	return pinnedInstaller().install(ctx, root, progress)
}

// DownloadForced re-downloads and replaces the pinned build even when a
// complete cache exists. The setup helper uses it for -Force after a damaged
// cache passed the presence check.
func DownloadForced(ctx context.Context, root string, progress func(string)) (string, error) {
	return pinnedInstaller().installWith(ctx, root, progress, true)
}

func pinnedInstaller() installer {
	return installer{client: &http.Client{}, releaseURL: releaseURL,
		expectedDigest: "sha256:" + assetSHA256, expectedURL: assetURL}
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type installer struct {
	client         *http.Client
	releaseURL     string
	expectedDigest string
	expectedURL    string
	idleTimeout    time.Duration
	retryDelay     time.Duration
}

// The timer covers headers and each read separately, so a slow but active
// transfer has no total deadline. Cancellation also unblocks a stalled read.
type idleBody struct {
	io.ReadCloser
	timer   *time.Timer
	cancel  context.CancelFunc
	timeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

func (i installer) request(ctx context.Context, url string, offset int64) (*http.Response, error) {
	timeout := i.idleTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(timeout, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	req.Header.Set("User-Agent", "MirrPane-FFmpeg-Setup")
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := i.client.Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	resp.Body = &idleBody{ReadCloser: resp.Body, timer: timer, cancel: cancel, timeout: timeout}
	return resp, nil
}

func (i installer) backoff(ctx context.Context, attempt int) error {
	delay := i.retryDelay
	if delay <= 0 {
		delay = time.Second
	}
	timer := time.NewTimer(delay * time.Duration(1<<attempt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (i installer) get(ctx context.Context, url string) (*http.Response, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := i.backoff(ctx, attempt-1); err != nil {
				return nil, err
			}
		}
		resp, err := i.request(ctx, url, 0)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				return resp, nil
			}
			resp.Body.Close()
			err = fmt.Errorf("FFmpeg pinned release %s: HTTP %d from %s", releaseTag, resp.StatusCode, url)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return nil, err
			}
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func (i installer) install(ctx context.Context, root string, progress func(string)) (string, error) {
	return i.installWith(ctx, root, progress, false)
}

func (i installer) installWith(ctx context.Context, root string, progress func(string), force bool) (string, error) {
	report := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	report("正在查詢固定版本 FFmpeg " + releaseTag + "…")
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
		return "", fmt.Errorf("FFmpeg pinned release %s is missing %s or valid size/SHA256 metadata", releaseTag, assetName)
	}
	if asset.Digest != i.expectedDigest || asset.URL != i.expectedURL {
		return "", fmt.Errorf("FFmpeg pinned asset URL/SHA256 mismatch in release %s; refusing changed upstream content", releaseTag)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("FFmpeg cache: %w", err)
	}
	name := "runtime-" + digest[:16]
	destination := filepath.Join(root, name)
	if force || !installed(destination) {
		stage, err := os.MkdirTemp(root, ".download-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(stage)
		report("正在下載 FFmpeg（首次啟動可能需要幾分鐘）…")
		archivePath := filepath.Join(stage, "runtime.zip")
		if err := i.download(ctx, asset, archivePath, report); err != nil {
			return "", err
		}
		report("正在驗證與安裝 FFmpeg…")
		dllDir := filepath.Join(stage, "bin")
		if err := extract(archivePath, dllDir); err != nil {
			return "", err
		}
		// Publish only a complete, verified runtime. Windows cannot rename onto
		// a non-empty directory, so a damaged entry (for example one missing a
		// DLL or holding a corrupted non-empty DLL) is moved aside first and the
		// new, verified directory takes its place. A concurrent instance may
		// have published good content while this download was in flight; its
		// result is reused and the damaged directory is discarded.
		if err := publish(dllDir, destination, force); err != nil {
			return "", err
		}
	}
	if err := writeMarker(root, name); err != nil {
		return "", err
	}
	report("FFmpeg 已就緒")
	return filepath.Abs(destination)
}

// writeMarker atomically points root/current at name. Concurrent installers
// may replace it at the same moment; Windows can report a sharing violation
// for the replacement, so retry and accept an identical value.
func writeMarker(root, name string) error {
	marker, err := os.CreateTemp(root, ".current-")
	if err != nil {
		return err
	}
	defer os.Remove(marker.Name())
	_, writeErr := marker.WriteString(name + "\n")
	closeErr := marker.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	destination := filepath.Join(root, "current")
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		if err := os.Rename(marker.Name(), destination); err == nil {
			return nil
		} else {
			last = err
		}
		if data, err := os.ReadFile(destination); err == nil && strings.TrimSpace(string(data)) == name {
			return nil // another installer already pointed it here
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return last
}

// publish swaps a verified stage into destination. With force, even a complete
// directory is replaced; otherwise a complete one already present (from an
// earlier run or a concurrent installer) is kept.
func publish(stage, destination string, force bool) error {
	if !force && installed(destination) {
		return nil
	}
	aside := ""
	if _, err := os.Lstat(destination); err == nil {
		aside = fmt.Sprintf("%s.stale-%d", destination, time.Now().UnixNano())
		if err := os.Rename(destination, aside); err != nil {
			if !force && installed(destination) {
				return nil // repaired by another process while we downloaded
			}
			if _, statErr := os.Lstat(destination); os.IsNotExist(statErr) {
				aside = "" // another installer moved the damaged copy away first
			} else {
				return fmt.Errorf("FFmpeg install: cannot move damaged runtime %s aside: %w", destination, err)
			}
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		// A concurrent installer can publish its complete directory between our
		// Lstat and this rename; Windows then refuses to replace a non-empty
		// directory. Reuse that result instead of failing the install.
		if installed(destination) {
			if aside != "" {
				os.RemoveAll(aside)
			}
			return nil
		}
		if aside != "" {
			if _, statErr := os.Lstat(destination); os.IsNotExist(statErr) {
				if restoreErr := os.Rename(aside, destination); restoreErr != nil {
					return fmt.Errorf("FFmpeg install: %w; the previous runtime is kept at %s (restore failed: %v)", err, aside, restoreErr)
				}
			} else {
				return fmt.Errorf("FFmpeg install: %w; the previous runtime is kept at %s", err, aside)
			}
		}
		return fmt.Errorf("FFmpeg install: %w", err)
	}
	if aside != "" {
		os.RemoveAll(aside)
	}
	return nil
}

func (i installer) download(ctx context.Context, asset releaseAsset, destination string, report func(string)) error {
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var offset int64
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if report != nil {
				report(fmt.Sprintf("FFmpeg 下載中斷，正在重試（%d/2）…", attempt))
			}
			if err := i.backoff(ctx, attempt-1); err != nil {
				return err
			}
		}
		resp, err := i.request(ctx, asset.URL, offset)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			// Servers without Range support return the entire object again.
			if err := f.Truncate(0); err != nil {
				resp.Body.Close()
				return err
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				resp.Body.Close()
				return err
			}
			offset = 0
		} else if resp.StatusCode == http.StatusPartialContent {
			if resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, asset.Size-1, asset.Size) {
				resp.Body.Close()
				return errors.New("FFmpeg download: invalid Content-Range; refusing unsafe resume")
			}
		} else {
			resp.Body.Close()
			last = fmt.Errorf("FFmpeg pinned asset %s: HTTP %d", assetName, resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return last
			}
			continue
		}
		buffer := make([]byte, 128<<10)
		lastReport := time.Time{}
		for {
			n, readErr := resp.Body.Read(buffer)
			if n > 0 {
				if offset+int64(n) > asset.Size {
					resp.Body.Close()
					return errors.New("FFmpeg download exceeds pinned asset size")
				}
				if _, err := f.Write(buffer[:n]); err != nil {
					resp.Body.Close()
					return err
				}
				offset += int64(n)
				if report != nil && (time.Since(lastReport) >= 250*time.Millisecond || offset == asset.Size) {
					report(fmt.Sprintf("正在下載 FFmpeg：%d / %d bytes（%.1f%%）", offset, asset.Size, float64(offset)*100/float64(asset.Size)))
					lastReport = time.Now()
				}
			}
			if readErr != nil {
				last = readErr
				break
			}
		}
		resp.Body.Close()
		if offset == asset.Size {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if offset != asset.Size {
		return fmt.Errorf("FFmpeg download incomplete after 3 attempts (%d/%d bytes): %w", offset, asset.Size, last)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
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
	hashes := make(map[string]string)
	var total uint64
	for _, entry := range z.File {
		name := path.Base(entry.Name)
		// BtbN calls its GPLv3 COPYING text LICENSE.txt in this pinned zip.
		wanted := name == "LICENSE.txt"
		if wanted {
			name = "COPYING"
		}
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
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(source, 256<<20+1))
		err = errors.Join(copyErr, f.Close(), source.Close())
		if err != nil || n == 0 || uint64(n) != entry.UncompressedSize64 {
			return fmt.Errorf("FFmpeg archive: invalid %s: %w", name, errors.Join(err, io.ErrUnexpectedEOF))
		}
		files[name] = true
		hashes[name] = hex.EncodeToString(h.Sum(nil))
	}
	if !complete(destination) {
		return fmt.Errorf("pinned FFmpeg shared build has an unsupported ABI (requires %s)", strings.Join(DLLs[:], ", "))
	}
	if !files["COPYING"] {
		return errors.New("FFmpeg archive is missing its GPL COPYING/LICENSE.txt")
	}
	// Write the manifest last: the directory is only publishable once every
	// file it records is on disk and hashed.
	return writeManifest(destination, hashes)
}

func writeManifest(dir string, hashes map[string]string) error {
	names := make([]string, 0, len(hashes))
	for name := range hashes {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", hashes[name], name)
	}
	return os.WriteFile(filepath.Join(dir, manifestName), []byte(b.String()), 0o644)
}
