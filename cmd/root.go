package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/kataras/golog"
	"github.com/mitchellh/go-wordwrap"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/AlbinoGeek/sc2-rsu/cmd/gui"
	"github.com/AlbinoGeek/sc2-rsu/sc2replaystats"
	"github.com/AlbinoGeek/sc2-rsu/sc2utils"
	"github.com/AlbinoGeek/sc2-rsu/utils"
)

const validReplaySize = 26 * 1024

var (
	// GUI is the application's graphical interface
	GUI *gui.GraphicalInterface

	rootCmd = &cobra.Command{
		Short: "SC2ReplayStats Uploader",
		Long:  `Unofficial SC2ReplayStats Uploader by AlbinoGeek`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if viper.GetBool("update.check.enabled") {
				go checkUpdateEvery(getUpdateDuration())
			}

			if !textMode {
				GUI = gui.New()
				GUI.Init(map[string]gui.Window{
					WindowMain:     &windowMain{WindowBase: &gui.WindowBase{App: GUI.App, UI: GUI}},
					WindowAbout:    &windowAbout{WindowBase: &gui.WindowBase{App: GUI.App, UI: GUI}},
					WindowSettings: &windowSettings{WindowBase: &gui.WindowBase{App: GUI.App, UI: GUI}},
				}, WindowMain)
				GUI.Run()
				return nil
			}

			key := viper.GetString("apikey")
			if key == "" {
				return errors.New("no API key in configuration, please use the login command")
			}

			if !sc2replaystats.ValidAPIKey(key) {
				return errors.New("invalid API key in configuration, please replace it or use the login command")
			}

			paths, err := getWatchPaths()
			if err != nil {
				return err
			}

			golog.Info("Starting Automatic Replay Uploader...")
			sc2api = sc2replaystats.New(key)

			done := make(chan struct{})
			w, err := newWatcher(paths)
			if err != nil {
				return err
			}
			defer w.Close()

			go func() {
				for {
					select {
					case event, ok := <-w.Events:
						if !ok {
							return
						}

						if event.Op&fsnotify.Create == fsnotify.Create {
							// bug: SC2 sometime writes out ".SC2Replay.writeCacheBackup" files
							if strings.HasSuffix(event.Name, "eplay") {
								go handleReplay(event.Name)
							}
						}
					case err, ok := <-w.Errors:
						if !ok {
							return
						}

						golog.Warnf("fswatcher error: %v", err)
					}
				}
			}()

			// Setup Interrupt (Ctrl+C) Handler
			c := make(chan os.Signal)
			signal.Notify(c, os.Interrupt, syscall.SIGTERM)
			go func() {
				sig := <-c
				fmt.Println()
				golog.Warnf("Received signal:%v, Quitting.", sig)
				close(done)
			}()

			golog.Debugf("Startup took: %v", time.Since(startTime))
			golog.Info("Ready!")
			<-done

			return nil
		},
	}
	sc2api    *sc2replaystats.Client
	startTime = time.Now()
	termWidth = 80
	textMode  bool
)

func findReplaysRoot() (string, error) {
	scanRoot := "/"
	if home, err := os.UserHomeDir(); err == nil {
		scanRoot = home
	}

	roots, err := sc2utils.FindReplaysRoot(scanRoot)
	if err != nil || len(roots) == 0 {
		return "", fmt.Errorf("unable to automatically determine the path to your replays directory: %v", err)
	}

	root := roots[0]

	if len(roots) > 1 {
		line := strings.Repeat("=", termWidth/2)
		fmt.Printf("\n%s\n%s\n", line, wordwrap.WrapString("More than one possible replay directory was located while we scanned for your StarCraft II installation's Accounts folder.\n\nPlease select which directory we should be watching below:", uint(termWidth/2)))

		for i, p := range roots {
			fmt.Printf("\n  %d: %s", 1+i, p)
		}

		fmt.Printf("\n%s\n", line)

		consoleReader := bufio.NewReaderSize(os.Stdin, 1)

		for {
			fmt.Printf("Your Choice [1-%d]: ", len(roots))

			input, _, _ := consoleReader.ReadLine()
			choice, err := strconv.Atoi(string(input))

			if err == nil && choice > 0 && choice-1 < len(roots) {
				root = roots[choice-1]
				break
			}
		}
	}

	return root, nil
}

func getWatchPaths() ([]string, error) {
	replaysRoot := viper.GetString("replaysRoot")
	if f, err := os.Stat(replaysRoot); err != nil || !f.IsDir() {
		golog.Warn("Replay Root not configured correctly, searching for replays directory...")
		golog.Info("Determining replays directory... (this could take a few minutes)...")

		root, err := findReplaysRoot()
		if err != nil {
			golog.Fatal(err)
		}

		viper.Set("replaysRoot", root)

		if err := saveConfig(); err != nil {
			return nil, err
		}

		golog.Infof("Using replays directory: %v", root)
		replaysRoot = root
	}

	accs, err := sc2utils.EnumerateAccounts(replaysRoot)
	golog.Debugf("account scan returned: %v toons", len(accs))

	if err != nil {
		return nil, fmt.Errorf("getWatchPaths: %v", err)
	}

	paths := make([]string, 0)

	for _, a := range accs {
		p := filepath.Join(replaysRoot, a, "Replays", "Multiplayer")
		if f, err := os.Stat(p); err == nil && f.IsDir() {
			paths = append(paths, p)
		}
	}

	return paths, nil
}

func handleReplay(replayFilename string) {
	golog.Debugf("uploading replay: %v", replayFilename)

	// wait for the replay to have finished being written (large enough filesize)
	var lastSize int64

	for {
		time.Sleep(time.Millisecond * 250)

		// ! smallest replay I've seen is 27418 bytes (-3 second long)
		if s, err := os.Stat(replayFilename); err == nil && s.Size() > validReplaySize {
			// check that the replay has stopped growing
			if s.Size() > lastSize {
				lastSize = s.Size()
			} else {
				break
			}
		}
	}

	rqid, err := sc2api.UploadReplay(replayFilename)
	_, mapName, _ := utils.SplitFilepath(replayFilename)

	if err != nil {
		golog.Errorf("failed to upload replay: %v: %v", mapName, err)
		return
	}

	golog.Infof("sc2replaystats accepted : [%v] %s", rqid, mapName)

	go watchReplayStatus(rqid)
}

func newWatcher(paths []string) (w *fsnotify.Watcher, err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to setup fswatcher: %v", err)
	}

	for _, p := range paths {
		golog.Debugf("Watching replays directory: %v", p)

		if err = watcher.Add(p); err != nil {
			golog.Fatalf("failed to watch replay directory: %v: %v", p, err)
		}
	}

	return watcher, nil
}

func watchReplayStatus(rqid string) {
	for {
		time.Sleep(time.Second)

		rid, err := sc2api.GetReplayStatus(rqid)

		if err != nil {
			golog.Errorf("error checking reply status: %v: %v", rqid, err)
			return // could not check status
		}

		if rid != "" {
			golog.Infof("sc2replaystats processed: [%v] %s", rqid, rid)
			return // replay parsed!
		}

		golog.Debugf("sc2replaystats process..: [%v] %s", rqid, rid)
	}
}
