package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/clementauger/tor-prebuilt/embedded"
	"github.com/cretz/bine/tor"
	"github.com/cretz/bine/torutil"
	tued25519 "github.com/cretz/bine/torutil/ed25519"
)

func getOrCreatePK(fpath string) (ed25519.PrivateKey, error) {
	var privateKey ed25519.PrivateKey
	if _, err := os.Stat(fpath); os.IsNotExist(err) {
		_, privateKey, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		x509Encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			return nil, err
		}
		pemEncoded := pem.EncodeToMemory(&pem.Block{Type: "ED25519 PRIVATE KEY", Bytes: x509Encoded})
		ioutil.WriteFile(fpath, pemEncoded, os.ModePerm)
	} else {
		d, _ := ioutil.ReadFile(fpath)
		block, _ := pem.Decode(d)
		x509Encoded := block.Bytes
		tPk, err := x509.ParsePKCS8PrivateKey(x509Encoded)
		if err != nil {
			return nil, err
		}
		if x, ok := tPk.(ed25519.PrivateKey); ok {
			privateKey = x
		} else {
			return nil, fmt.Errorf("invalid key type %T wanted ed25519.PrivateKey", tPk)
		}
	}
	return privateKey, nil
}

type torServer struct {
	Handler http.Handler
	// PrivateKey path to a pem encoded ed25519 private key
	PrivateKey ed25519.PrivateKey
}

func onionAddr(pk ed25519.PrivateKey) string {
	return torutil.OnionServiceIDFromV3PublicKey(tued25519.PublicKey([]byte(pk.Public().(ed25519.PublicKey))))
}

func (ts *torServer) ListenAndServe() error {

	d, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}

	// Start tor with default config (can set start conf's DebugWriter to os.Stdout for debug logs)
	// fmt.Println("Starting and registering onion service, please wait a couple of minutes...")
	t, err := tor.Start(nil, &tor.StartConf{TempDataDirBase: d, ProcessCreator: embedded.NewCreator(), NoHush: true})
	if err != nil {
		return fmt.Errorf("unable to start Tor: %v", err)
	}
	defer t.Close()

	// Wait at most a few minutes to publish the service
	listenCtx, listenCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer listenCancel()
	// Create a v3 onion service to listen on any port but show as 80
	onion, err := t.Listen(listenCtx, &tor.ListenConf{Key: ts.PrivateKey, Version3: true, RemotePorts: []int{80}})
	if err != nil {
		return fmt.Errorf("unable to create onion service: %v", err)
	}
	defer onion.Close()

	// fmt.Printf("server listening at http://%v.onion\n", onion.ID)

	return http.Serve(onion, ts.Handler)
}