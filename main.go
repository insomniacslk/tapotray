package main

import (
	"fmt"
	"log"
	"os"

	"fyne.io/systray"
)

const progname = "tapotray"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "edit":
			if err := editConfig(); err != nil {
				log.Fatalf("Failed to edit config file: %v", err)
			}
		default:
			fmt.Fprintf(os.Stderr, "Unknown command %q\n\n", os.Args[1])
			fmt.Fprintf(os.Stderr, "Usage: %s [command]\n\n", progname)
			fmt.Fprintf(os.Stderr, "Commands:\n")
			fmt.Fprintf(os.Stderr, "  edit    open the configuration file in EDITOR and validate it\n\n")
			fmt.Fprintf(os.Stderr, "With no command, run the tray app.\n")
			os.Exit(1)
		}
		return
	}
	_, cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to open config file: %v", err)
	}
	// Fyne needs the main thread for its event loop, so the tray runs with
	// an external loop.
	a := initWindow(cfg)
	start, end := systray.RunWithExternalLoop(func() { onReady(cfg) }, onExit)
	start()
	a.Run()
	end()
}
