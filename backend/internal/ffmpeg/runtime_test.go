package ffmpeg

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func archive(t *testing.T, names []string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	z := zip.NewWriter(&buffer)
	for _, name := range names {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("test DLL content: " + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func sharedArchive(t *testing.T) []byte {
	t.Helper()
	names := []string{"ffmpeg/LICENSE.txt"}
	for _, dll := range DLLs {
		names = append(names, "ffmpeg/bin/"+dll)
	}
	return archive(t, names)
}

func testInstaller(t *testing.T, data []byte, mutate func(*releaseAsset), downloads *atomic.Int32) installer {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive" {
			downloads.Add(1)
			_, _ = w.Write(data)
			return
		}
		digest := sha256.Sum256(data)
		asset := releaseAsset{Name: assetName, URL: server.URL + "/archive", Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(data))}
		if mutate != nil {
			mutate(&asset)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Assets []releaseAsset `json:"assets"`
		}{[]releaseAsset{asset}})
	}))
	t.Cleanup(server.Close)
	digest := sha256.Sum256(data)
	return installer{client: server.Client(), releaseURL: server.URL + "/pinned",
		expectedURL: server.URL + "/archive", expectedDigest: "sha256:" + hex.EncodeToString(digest[:]), retryDelay: time.Millisecond}
}

func TestInstallVerifiedRuntimeAndReuse(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	root := t.TempDir()
	dir, err := i.install(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !complete(dir) || cached(root) != dir {
		t.Fatalf("runtime not published: %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime.zip")); !os.IsNotExist(err) {
		t.Fatal("download archive was retained in runtime")
	}
	again, err := i.install(context.Background(), root, nil)
	if err != nil || again != dir || downloads.Load() != 1 {
		t.Fatalf("cache reuse: %s %v, downloads=%d", again, err, downloads.Load())
	}
}

func TestInstallRepairsMissingLicense(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	root := t.TempDir()
	dir, err := i.install(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "COPYING")); err != nil {
		t.Fatal(err)
	}
	if _, err := i.install(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "COPYING")); err != nil || info.Size() == 0 || downloads.Load() != 2 {
		t.Fatalf("license not repaired: %v, downloads=%d", err, downloads.Load())
	}
}

func TestInstallRejectsCorruptOrIncompleteDownloads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		mutate func(*releaseAsset)
	}{
		{"digest", sharedArchive(t), func(a *releaseAsset) { a.Digest = "sha256:" + strings.Repeat("0", 64) }},
		{"truncated", sharedArchive(t), func(a *releaseAsset) { a.Size += 5 }},
		{"missing-digest", sharedArchive(t), func(a *releaseAsset) { a.Digest = "" }},
		{"oversize", sharedArchive(t), func(a *releaseAsset) { a.Size = maxArchive + 1 }},
		{"not-zip", []byte("invalid zip"), nil},
		{"incompatible-abi", archive(t, []string{"ffmpeg/bin/avcodec-64.dll"}), nil},
		{"missing-dll", archive(t, []string{"ffmpeg/bin/avcodec-63.dll"}), nil},
		{"missing-license", archive(t, DLLs[:]), nil},
		{"duplicate-dll", archive(t, []string{"a/avcodec-63.dll", "b/avcodec-63.dll"}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var downloads atomic.Int32
			i := testInstaller(t, tc.data, tc.mutate, &downloads)
			root := t.TempDir()
			if _, err := i.install(context.Background(), root, nil); err == nil {
				t.Fatal("bad runtime accepted")
			}
			if cached(root) != "" {
				t.Fatal("bad runtime published")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed download left files: %v %v", entries, err)
			}
		})
	}
}

func TestExtractNeverWritesArchivePaths(t *testing.T) {
	root := t.TempDir()
	names := []string{"../../escaped.txt", "/absolute.txt", "ffmpeg/LICENSE.txt"}
	for _, dll := range DLLs {
		names = append(names, "../../"+dll)
	}
	data := archive(t, names)
	input := filepath.Join(root, "archive.zip")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "safe")
	if err := extract(input, dest); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 || !complete(dest) {
		t.Fatalf("unsafe extraction: %v %v", entries, err)
	}
}

func TestExplicitOverrideAndPartialRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MIRRPANE_FFMPEG_DIR", dir)
	for i, dll := range DLLs {
		if _, err := Find(); err == nil {
			t.Fatalf("accepted incomplete runtime with %d DLLs", i)
		}
		if err := os.WriteFile(filepath.Join(dir, dll), []byte("DLL"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := Find(); err != nil || got != dir {
		t.Fatalf("Find()=%s %v", got, err)
	}
}

func TestCanceledInstallationAndInvalidOverrideDoNotDownload(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := i.install(ctx, t.TempDir(), nil); err == nil {
		t.Fatal("canceled install succeeded")
	}
	t.Setenv("MIRRPANE_FFMPEG_DIR", t.TempDir())
	if _, err := Ensure(context.Background(), t.TempDir(), nil, nil); err == nil {
		t.Fatal("invalid override ignored")
	}
	if downloads.Load() != 0 {
		t.Fatal("download occurred")
	}
}

func TestCacheMarkerCannotEscapeCache(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"runtime-../outside", "runtime-..\\outside", "../outside", "."} {
		if err := os.WriteFile(filepath.Join(root, "current"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if cached(root) != "" {
			t.Fatal("unsafe cache marker accepted")
		}
	}
}

func TestProductionPin(t *testing.T) {
	i := pinnedInstaller()
	if strings.Contains(i.releaseURL, "/latest") || i.expectedURL != assetURL || i.expectedDigest != "sha256:"+assetSHA256 || i.client.Timeout != 0 {
		t.Fatalf("invalid pin/client: %+v", i)
	}
}

func TestDownloadResumeAndRangeFallback(t *testing.T) {
	for _, mode := range []string{"resume", "ignore-range", "invalid-range", "corrupt", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte(strings.Repeat("verified download", 128))
			digest := sha256.Sum256(data)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if mode == "unavailable" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if call == 1 && mode != "corrupt" {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					_, _ = w.Write(data[:100]) // net/http closes a short response.
					return
				}
				if mode == "corrupt" {
					_, _ = w.Write(bytes.Repeat([]byte{'x'}, len(data)))
					return
				}
				if r.Header.Get("Range") != "bytes=100-" {
					t.Errorf("missing resume Range: %q", r.Header.Get("Range"))
				}
				if mode == "ignore-range" {
					_, _ = w.Write(data)
					return
				}
				start := 100
				if mode == "invalid-range" {
					start = 99
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start:])
			}))
			defer server.Close()
			i := installer{client: server.Client(), retryDelay: time.Millisecond}
			asset := releaseAsset{URL: server.URL, Size: int64(len(data)), Digest: "sha256:" + hex.EncodeToString(digest[:])}
			var progress []string
			dest := filepath.Join(t.TempDir(), "download")
			err := i.download(context.Background(), asset, dest, func(s string) { progress = append(progress, s) })
			wantError := mode == "invalid-range" || mode == "corrupt" || mode == "unavailable"
			if (err != nil) != wantError {
				t.Fatalf("download error=%v", err)
			}
			if !wantError {
				got, _ := os.ReadFile(dest)
				if !bytes.Equal(got, data) || calls.Load() != 2 || !strings.Contains(strings.Join(progress, "\n"), "100.0%") {
					t.Fatalf("resume/progress failed: calls=%d progress=%v", calls.Load(), progress)
				}
			}
			if mode == "unavailable" && calls.Load() != 3 {
				t.Fatalf("retry count=%d", calls.Load())
			}
		})
	}
}

func TestDownloadIdleTimeoutAndActiveTransfer(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprint(stall), func(t *testing.T) {
			data := []byte("abcdefghij")
			digest := sha256.Sum256(data)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				w.(http.Flusher).Flush()
				if stall {
					<-r.Context().Done()
					return
				}
				for _, b := range data {
					_, _ = w.Write([]byte{b})
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}))
			defer server.Close()
			i := installer{client: server.Client(), idleTimeout: 100 * time.Millisecond, retryDelay: time.Millisecond}
			asset := releaseAsset{URL: server.URL, Size: int64(len(data)), Digest: "sha256:" + hex.EncodeToString(digest[:])}
			err := i.download(context.Background(), asset, filepath.Join(t.TempDir(), "archive"), nil)
			if (err != nil) != stall {
				t.Fatalf("stall=%t error=%v", stall, err)
			}
		})
	}
}

func TestPinnedMetadataMissingAssetAndChangedURL(t *testing.T) {
	for _, mutate := range []func(*releaseAsset){
		func(a *releaseAsset) { a.Name = "other.zip" },
		func(a *releaseAsset) { a.URL += "/changed" },
	} {
		var downloads atomic.Int32
		i := testInstaller(t, sharedArchive(t), mutate, &downloads)
		if _, err := i.install(context.Background(), t.TempDir(), nil); err == nil || downloads.Load() != 0 {
			t.Fatalf("accepted changed/missing pinned asset: %v", err)
		}
	}
}

func TestMetadataRetriesAndBackoffCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	i := installer{client: server.Client(), retryDelay: time.Millisecond}
	resp, err := i.get(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := i.backoff(ctx, 0); err != context.Canceled {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestFindIgnoresPATHAndCWD(t *testing.T) {
	writeDummyDLLs := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, dll := range DLLs {
			if err := os.WriteFile(filepath.Join(dir, dll), []byte("planted"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	root := t.TempDir()
	pathDir := filepath.Join(root, "path-bin")
	writeDummyDLLs(pathDir)
	if err := os.WriteFile(filepath.Join(pathDir, "ffmpeg.exe"), []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}

	cwdDir := filepath.Join(root, "work")
	cwdBin := filepath.Join(cwdDir, "third_party", "ffmpeg", "bin")
	writeDummyDLLs(cwdBin)
	cwdCache := filepath.Join(cwdDir, "third_party", "ffmpeg", "runtime-deadbeef")
	writeDummyDLLs(cwdCache)
	if err := os.WriteFile(filepath.Join(cwdDir, "third_party", "ffmpeg", "current"), []byte("runtime-deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MIRRPANE_FFMPEG_DIR", "")
	t.Setenv("PATH", pathDir)
	t.Chdir(cwdDir)

	if dir, err := Find(); err == nil {
		t.Fatalf("Find() selected untrusted PATH/CWD directory %q", dir)
	}
}

func assertNoStagingLeftovers(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".download-") || strings.Contains(entry.Name(), ".stale-") {
			t.Fatalf("staging leftover after install: %s", entry.Name())
		}
	}
}

func readManifestLines(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// P2.5: a cache that is present but damaged (a missing DLL, a non-empty but
// corrupted DLL, a wrong licence, or a manifest that no longer matches) must be
// repaired by moving the damaged directory aside and publishing the freshly
// downloaded, verified one. Windows cannot rename onto a non-empty directory,
// so the earlier implementation left the missing file missing.
func TestInstallRepairsDamagedCache(t *testing.T) {
	for _, damage := range []string{"missing-dll", "corrupt-dll", "corrupt-license", "manifest-without-dll"} {
		t.Run(damage, func(t *testing.T) {
			var downloads atomic.Int32
			i := testInstaller(t, sharedArchive(t), nil, &downloads)
			root := t.TempDir()
			dir, err := i.install(context.Background(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !installed(dir) {
				t.Fatal("fresh install is not complete")
			}
			switch damage {
			case "missing-dll":
				if err := os.Remove(filepath.Join(dir, DLLs[1])); err != nil {
					t.Fatal(err)
				}
			case "corrupt-dll":
				if err := os.WriteFile(filepath.Join(dir, DLLs[3]), []byte("truncated but non-empty"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "corrupt-license":
				if err := os.WriteFile(filepath.Join(dir, "COPYING"), []byte("not the GPL text"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "manifest-without-dll":
				lines := readManifestLines(t, dir)
				var kept []string
				for _, line := range lines {
					if !strings.HasSuffix(line, "  "+DLLs[0]) {
						kept = append(kept, line)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if installed(dir) {
				t.Fatalf("%s was not detected as damaged", damage)
			}
			again, err := i.install(context.Background(), root, nil)
			if err != nil {
				t.Fatalf("repair failed: %v", err)
			}
			if again != dir || !installed(dir) || downloads.Load() != 2 {
				t.Fatalf("damage not repaired: dir=%s installed=%t downloads=%d", again, installed(dir), downloads.Load())
			}
			if got, err := os.ReadFile(filepath.Join(dir, DLLs[3])); err != nil || !bytes.Equal(got, []byte("test DLL content: ffmpeg/bin/"+DLLs[3])) {
				t.Fatalf("DLL content not restored: %q %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "COPYING")); err != nil || string(got) != "test DLL content: ffmpeg/LICENSE.txt" {
				t.Fatalf("licence not restored: %q %v", got, err)
			}
			assertNoStagingLeftovers(t, root)
		})
	}
}

func TestConcurrentInstallsPublishOneVerifiedRuntime(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	root := t.TempDir()
	const workers = 4
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, errs[n] = i.install(context.Background(), root, nil)
		}(n)
	}
	wg.Wait()
	for n, err := range errs {
		if err != nil {
			t.Fatalf("installer %d: %v", n, err)
		}
	}
	dir := cached(root)
	if dir == "" || !installed(dir) {
		t.Fatalf("no verified runtime published (downloads=%d)", downloads.Load())
	}
	assertNoStagingLeftovers(t, root)
}

func TestManifestMustCoverEveryShippedDLL(t *testing.T) {
	dir := t.TempDir()
	hashes := make(map[string]string)
	for _, dll := range DLLs {
		data := []byte("dll " + dll)
		if err := os.WriteFile(filepath.Join(dir, dll), data, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		hashes[dll] = hex.EncodeToString(sum[:])
	}
	// Legacy directories without a manifest are accepted on presence alone.
	if !complete(dir) {
		t.Fatal("legacy cache without a manifest rejected")
	}
	// A manifest is authoritative: it must list every shipped DLL and match.
	partial := map[string]string{DLLs[0]: hashes[DLLs[0]]}
	if err := writeManifest(dir, partial); err != nil {
		t.Fatal(err)
	}
	if complete(dir) {
		t.Fatal("manifest missing a DLL accepted")
	}
	if err := writeManifest(dir, hashes); err != nil {
		t.Fatal(err)
	}
	if !complete(dir) {
		t.Fatal("valid manifest rejected")
	}
	if err := os.WriteFile(filepath.Join(dir, DLLs[2]), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if complete(dir) {
		t.Fatal("tampered DLL accepted")
	}
}

func TestLegacyPinnedCacheRequiresVerifiedContents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime-"+assetSHA256[:16])
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"COPYING"}, DLLs[:]...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("incomplete previous installation"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if installed(dir) {
		t.Fatal("legacy pinned cache accepted unrelated file contents")
	}
}

// P2.5: when the cached copy passes the presence check but fails the ABI
// validator, Ensure replaces it with a fresh verified download instead of
// returning the broken directory forever.
func TestEnsureReplacesRuntimeThatFailsValidation(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	root := t.TempDir()
	dir, err := i.install(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate content that looks installed but is ABI-incompatible: the
	// validator rejects the old tree until it has been replaced.
	stale := filepath.Join(dir, "abi-mismatch.flag")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRRPANE_FFMPEG_DIR", "")
	validate := func(dir string) error {
		if _, err := os.Stat(filepath.Join(dir, "abi-mismatch.flag")); err == nil {
			return errors.New("decode: incompatible ABI")
		}
		return nil
	}
	got, err := ensure(context.Background(), root, nil, validate, i)
	if err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 2 {
		t.Fatalf("validation failure did not force a re-download: downloads=%d", downloads.Load())
	}
	if got != dir || !installed(got) {
		t.Fatalf("repaired runtime not used: %s", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale directory content survived the repair")
	}
	assertNoStagingLeftovers(t, root)
}

// A compatible cache must not be re-downloaded when the network is unavailable.
func TestEnsureUsesGoodCacheOffline(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	root := t.TempDir()
	dir, err := i.install(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	offline := i
	offline.releaseURL = "http://127.0.0.1:1/pinned"
	offline.expectedURL = "http://127.0.0.1:1/archive"
	offline.retryDelay = time.Millisecond
	t.Setenv("MIRRPANE_FFMPEG_DIR", "")
	got, err := ensure(context.Background(), root, nil, func(string) error { return nil }, offline)
	if err != nil || got != dir || downloads.Load() != 1 {
		t.Fatalf("good cache was not reused offline: %s %v, downloads=%d", got, err, downloads.Load())
	}
}

// -Force must replace a complete pinned cache, not silently reuse it.
func TestForcedInstallReplacesCompleteCache(t *testing.T) {
	var downloads atomic.Int32
	i := testInstaller(t, sharedArchive(t), nil, &downloads)
	root := t.TempDir()
	dir, err := i.install(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.flag"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := i.installWith(context.Background(), root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if again != dir || downloads.Load() != 2 || !installed(dir) {
		t.Fatalf("forced install reused the cache: downloads=%d", downloads.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.flag")); !os.IsNotExist(err) {
		t.Fatal("forced install did not replace the directory")
	}
	assertNoStagingLeftovers(t, root)
}

// Supplementary: the manifest lines are sorted so identical content produces
// an identical cache directory listing.
func TestManifestIsSorted(t *testing.T) {
	hashes := map[string]string{"b": strings.Repeat("1", 64), "a": strings.Repeat("2", 64)}
	dir := t.TempDir()
	if err := writeManifest(dir, hashes); err != nil {
		t.Fatal(err)
	}
	lines := readManifestLines(t, dir)
	names := make([]string, len(lines))
	for i, line := range lines {
		names[i] = strings.Fields(line)[1]
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("manifest not sorted: %v", names)
	}
}
