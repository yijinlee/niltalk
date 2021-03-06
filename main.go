// Niltalk, April 2015
// License AGPL3

package main

import (
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	rice "github.com/GeertJohan/go.rice"
	"github.com/fsnotify/fsnotify"
	"github.com/go-chi/chi"
	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/niltalk/internal/hub"
	"github.com/knadh/niltalk/internal/notify"
	"github.com/knadh/niltalk/internal/upload"
	"github.com/knadh/niltalk/store"
	"github.com/knadh/niltalk/store/fs"
	"github.com/knadh/niltalk/store/mem"
	"github.com/knadh/niltalk/store/redis"
	flag "github.com/spf13/pflag"
)

var (
	logger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
	ko     = koanf.New(".")

	// Version of the build injected at build time.
	buildString = "unknown"
)

// App is the global app context that's passed around.
type App struct {
	hub    *hub.Hub
	cfg    *hub.Config
	tpl    *template.Template
	tplBox *rice.Box
	jit    bool
	logger *log.Logger
}

func loadConfig() {
	// Register --help handler.
	f := flag.NewFlagSet("config", flag.ContinueOnError)
	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}
	f.StringSlice("config", []string{"config.toml"},
		"Path to one or more TOML config files to load in order")
	f.Bool("new-config", false, "generate sample config file")
	f.Bool("new-unit", false, "generate systemd unit file")
	f.Bool("onion", false, "Show the onion URL")
	f.Bool("version", false, "Show build version")
	f.Bool("jit", defaultJIT, "build templates just in time")
	f.Parse(os.Args[1:])

	// Display version.
	if ok, _ := f.GetBool("version"); ok {
		fmt.Println(buildString)
		os.Exit(0)
	}

	// Generate new config.
	if ok, _ := f.GetBool("new-config"); ok {
		if err := newConfigFile(); err != nil {
			logger.Println(err)
			os.Exit(1)
		}
		logger.Println("generated config.toml. Edit and run the app.")
		os.Exit(0)
	}

	// Generate new unit.
	if ok, _ := f.GetBool("new-unit"); ok {
		if err := newUnitFile(); err != nil {
			logger.Println(err)
			os.Exit(1)
		}
		logger.Println("generated niltalk.service. Edit and install the service.")
		os.Exit(0)
	}

	// Read the config files.
	cFiles, _ := f.GetStringSlice("config")
	for _, f := range cFiles {
		logger.Printf("reading config: %s", f)
		if err := ko.Load(file.Provider(f), toml.Parser()); err != nil {
			if os.IsNotExist(err) {
				logger.Fatal("config file not found. If there isn't one yet, run --new-config to generate one.")
			}
			logger.Fatalf("error loadng config from file: %v.", err)
		}
	}

	// Merge env flags into config.
	if err := ko.Load(env.Provider("NILTALK_", ".", func(s string) string {
		return strings.Replace(strings.ToLower(
			strings.TrimPrefix(s, "NILTALK_")), "__", ".", -1)
	}), nil); err != nil {
		logger.Printf("error loading env config: %v", err)
	}

	// Merge command line flags into config.
	ko.Load(posflag.Provider(f, ".", ko), nil)
}

func newConfigFile() error {
	if _, err := os.Stat("config.toml"); !os.IsNotExist(err) {
		return errors.New("config.toml exists. Remove it to generate a new one")
	}

	// Initialize the static file system into which all
	// required static assets (.sql, .js files etc.) are loaded.
	sampleBox := rice.MustFindBox("static/samples")
	b, err := sampleBox.Bytes("config.toml")
	if err != nil {
		return fmt.Errorf("error reading sample config (is binary stuffed?): %v", err)
	}

	return ioutil.WriteFile("config.toml", b, 0644)
}

func newUnitFile() error {
	if _, err := os.Stat("niltalk.service"); !os.IsNotExist(err) {
		return errors.New("niltalk.service exists. Remove it to generate a new one")
	}

	// Initialize the static file system into which all
	// required static assets (.sql, .js files etc.) are loaded.
	sampleBox := rice.MustFindBox("static/samples")
	b, err := sampleBox.Bytes("niltalk.service")
	if err != nil {
		return fmt.Errorf("error reading sample unit (is binary stuffed?): %v", err)
	}

	return ioutil.WriteFile("niltalk.service", b, 0644)
}

func main() {
	// Load configuration from files.
	loadConfig()

	// Load file system boxes
	rConf := rice.Config{LocateOrder: []rice.LocateMethod{rice.LocateWorkingDirectory, rice.LocateAppended}}
	tplBox := rConf.MustFindBox("static/templates")
	assetBox := rConf.MustFindBox("static/static")

	// Initialize global app context.
	app := &App{
		logger: logger,
		tplBox: tplBox,
	}
	if err := ko.Unmarshal("app", &app.cfg); err != nil {
		logger.Fatalf("error unmarshalling 'app' config: %v", err)
	}

	minTime := time.Duration(3) * time.Second
	if app.cfg.RoomAge < minTime || app.cfg.WSTimeout < minTime {
		logger.Fatal("app.websocket_timeout and app.roomage should be > 3s")
	}

	// Initialize store.
	var store store.Store
	if app.cfg.Storage == "redis" {
		var storeCfg redis.Config
		if err := ko.Unmarshal("store", &storeCfg); err != nil {
			logger.Fatalf("error unmarshalling 'store' config: %v", err)
		}

		s, err := redis.New(storeCfg)
		if err != nil {
			log.Fatalf("error initializing store: %v", err)
		}
		store = s

	} else if app.cfg.Storage == "memory" {
		var storeCfg mem.Config
		if err := ko.Unmarshal("store", &storeCfg); err != nil {
			logger.Fatalf("error unmarshalling 'store' config: %v", err)
		}

		s, err := mem.New(storeCfg)
		if err != nil {
			log.Fatalf("error initializing store: %v", err)
		}
		store = s

	} else if app.cfg.Storage == "fs" {
		var storeCfg fs.Config
		if err := ko.Unmarshal("store", &storeCfg); err != nil {
			logger.Fatalf("error unmarshalling 'store' config: %v", err)
		}

		s, err := fs.New(storeCfg, logger)
		if err != nil {
			log.Fatalf("error initializing store: %v", err)
		}
		store = s
		defer s.Close()

	} else {
		logger.Fatal("app.storage must be one of redis|memory|fs")
	}

	if ko.Bool("onion") {
		pk, err := loadTorPK(app.cfg, store)
		if err != nil {
			logger.Fatalf("could not read or write the private key: %v", err)
		}
		fmt.Printf("http://%v.onion\n", onionAddr(pk))
		return // to allow for defers to execute
	}

	app.hub = hub.NewHub(app.cfg, store, logger)

	if err := ko.Unmarshal("rooms", &app.cfg.Rooms); err != nil {
		logger.Fatalf("error unmarshalling 'rooms' config: %v", err)
	}
	// setup predefined rooms
	for _, room := range app.cfg.Rooms {
		r, err := app.hub.AddPredefinedRoom(room.ID, room.Name, room.Password)
		if err != nil {
			logger.Printf("error creating a predefined room %q: %v", room.Name, err)
			continue
		}
		r.PredefinedUsers = make([]hub.PredefinedUser, len(room.Users), len(room.Users))
		copy(r.PredefinedUsers, room.Users)
		for _, u := range r.PredefinedUsers {
			if u.Growl {
				r.GrowlEnabler = append(r.GrowlEnabler, "@"+u.Name)
			}
		}
		if len(r.GrowlEnabler) > 0 {
			n := notify.New(room.Growl, app.cfg.RootURL, r.ID, app.logger, assetBox)
			if err = n.Init(); err != nil {
				logger.Printf("error setting up growl notifications for the predefined room %q: %v", room.Name, err)
				continue
			}
			r.GrowlHandler = n.OnGrowlMessage
		}
		_, err = app.hub.ActivateRoom(r.ID)
		if err != nil {
			logger.Printf("error activating a predefined room %q: %v", room.Name, err)
			continue
		}
	}

	// Compile static templates.
	tpl, err := app.buildTpl()
	if err != nil {
		logger.Fatalf("error compiling templates: %v", err)
	}
	app.jit = ko.Bool("jit")
	app.tpl = tpl

	// Setup the file upload store.
	var uploadCfg upload.Config
	if err := ko.Unmarshal("upload", &uploadCfg); err != nil {
		logger.Fatalf("error unmarshalling 'upload' config: %v", err)
	}

	uploadStore := upload.New(uploadCfg)
	if err := uploadStore.Init(); err != nil {
		logger.Fatalf("error initializing upload store: %v", err)
	}

	// Register HTTP routes.
	r := chi.NewRouter()
	r.Get("/", wrap(handleIndex, app, 0))
	r.Get("/r/{roomID}/ws", wrap(handleWS, app, hasAuth|hasRoom))

	// API.
	r.Post("/api/rooms", wrap(handleCreateRoom, app, 0))
	r.Post("/r/{roomID}/login", wrap(handleLogin, app, hasRoom))
	r.Delete("/r/{roomID}/login", wrap(handleLogout, app, hasAuth|hasRoom))

	r.Post("/r/{roomID}/upload", handleUpload(uploadStore))
	r.Get("/r/{roomID}/uploaded/{fileID}", handleUploaded(uploadStore))

	// Views.
	r.Get("/r/{roomID}", wrap(handleRoomPage, app, hasAuth|hasRoom))

	// Assets.
	assets := http.StripPrefix("/static/", http.FileServer(assetBox.HTTPBox()))
	r.Get("/static/*", assets.ServeHTTP)

	// Start the app.
	lnAddr := ko.String("app.address")
	ln, err := net.Listen("tcp", lnAddr)
	if err != nil {
		logger.Fatalf("couldn't listen address %q: %v", lnAddr, err)
	}

	if app.cfg.Tor {
		pk, err := loadTorPK(app.cfg, store)
		if err != nil {
			logger.Fatalf("could not read or write the private key: %v", err)
		}

		srv := &torServer{
			PrivateKey: pk,
			Handler:    r,
		}
		logger.Printf("starting hidden service on http://%v.onion", onionAddr(pk))
		go func() {
			if err := srv.Serve(ln); err != nil {
				logger.Fatalf("couldn't serve: %v", err)
			}
		}()
	}

	srv := http.Server{
		Handler: r,
	}
	logger.Printf("starting server on http://%v", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil {
			logger.Fatalf("couldn't serve: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	var cFiles []string
	ko.Unmarshal("config", &cFiles)
	select {
	case <-fileWatcher(cFiles...):
	case sig := <-c:
		logger.Printf("shutting down: %v", sig)
	}
}

func fileWatcher(files ...string) chan struct{} {
	out := make(chan struct{})
	if len(files) > 0 {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			logger.Printf("failed to initialize configuration file watcher: %v", err)
			return out
		}
		for _, f := range files {
			err = watcher.Add(f)
			if err != nil {
				logger.Printf("failed to add configuration file %q watcher: %v", f, err)
			}
		}
		go func() {
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok {
						return
					}
					// if event.Op&fsnotify.Write == fsnotify.Write {
					logger.Printf("configuration file %q was modified", event.Name)
					out <- struct{}{}
					// }
				case err, ok := <-watcher.Errors:
					if !ok {
						return
					}
					logger.Printf("watcher error: %v", err)
				}
			}
		}()
	}
	return out
}

func (a *App) getTpl() (*template.Template, error) {
	if a.jit {
		return a.buildTpl()
	}
	return a.tpl, nil
}

func (a *App) buildTpl() (*template.Template, error) {
	tpl := template.New("")
	err := a.tplBox.Walk("/", func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		s, err := a.tplBox.String(path)
		if err != nil {
			return err
		}
		tpl, err = tpl.Parse(s)
		if err != nil {
			return err
		}
		return nil
	})
	return tpl, err
}
