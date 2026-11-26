// ffmpeg-setup downloads the runtime used by development and release packaging.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/ffmpeg"
)

func main() {
	output := flag.String("output", "../third_party/ffmpeg", "portable cache directory")
	force := flag.Bool("force", false, "re-download and replace the runtime even if a complete cache exists")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	progress := func(s string) { fmt.Fprintln(os.Stderr, s) }
	var dir string
	var err error
	if *force {
		dir, err = ffmpeg.DownloadForced(ctx, *output, progress)
	} else {
		dir, err = ffmpeg.DownloadLatest(ctx, *output, progress)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Emit the absolute DLL directory for build.ps1 and CI.
	dir, _ = filepath.Abs(dir)
	fmt.Println(dir)
}
