package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// appVersion is stamped by the packaging script with
// -ldflags "-X main.appVersion=<version>".
var appVersion = "dev"

func main() {
	installDirFlag := flag.String("install-dir", "", "override the MirrPane installation directory containing backend\\airplayd.exe")
	shmFlag := flag.String("shm", fmt.Sprintf(`Local\MirrPaneFrames-%d`, os.Getpid()), "shared-memory section for this receiver and its viewer")
	demoFlag := flag.Bool("demo", false, "render a synthetic test picture instead of the receiver's frames (no sidecar)")
	demoSettings := flag.Bool("demo-settings", false, "open the settings dialog at startup (UI testing)")
	flag.Parse()

	installDir := resolveInstallDir(*installDirFlag)
	var frameSrc FrameSource
	if *demoFlag {
		frameSrc = NewDemoFrameSource(2560, 1178)
	} else {
		frameSrc = newPlatformFrameSource(*shmFlag)
	}
	app := newMirrPaneApp(installDir, frameSrc, *demoFlag)
	app.sidecar.mapping = *shmFlag
	if *demoSettings {
		app.openSettings()
	}

	mygo.App.SetName("鏡屏 MirrPane")
	mygo.App.OnWindowAllClosed(func() {
		mygo.App.Quit()
	})
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		app.shutdown()
	})

	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:           "鏡屏 MirrPane",
			Width:           1280,
			Height:          800,
			MinWidth:        640,
			MinHeight:       420,
			BackgroundColor: "#191817",
			Content:         ui.View(app.view),
		})
		app.attachWindow(win)
	})

	if err := mygo.App.Run(); err != nil {
		app.shutdown()
		log.Fatalf("mirrpane: %v", err)
	}
	app.shutdown()
}

// resolveInstallDir finds the root folder holding backend\airplayd.exe.
// Development launches can point -install-dir at a staged build. A damaged
// portable installation must report its missing sidecar at its own location.
func resolveInstallDir(explicit string) string {
	if explicit != "" {
		if abs, err := filepath.Abs(explicit); err == nil {
			return abs
		}
		return explicit
	}

	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}
