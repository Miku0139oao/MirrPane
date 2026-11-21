package ffmpeg

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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
	var names []string
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
	return installer{client: server.Client(), releaseURL: server.URL + "/latest"}
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
	names := []string{"../../escaped.txt", "/absolute.txt"}
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
