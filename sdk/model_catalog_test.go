package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDecodeNativeModelCatalog(t *testing.T) {
	const ref = `{"provider":"fixture","modelId":"a"}`
	const row = `{"provider":"fixture","modelId":"a","label":"Fixture A","available":true,"reason":null,"thinkingLevels":["off"]}`
	const valid = `{"default":` + ref + `,"current":` + ref + `,"models":[` + row + `]}`
	if catalog, err := decodeModelCatalog([]byte(valid), false); err != nil || catalog.Current == nil || *catalog.Current != catalog.Models[0].Model {
		t.Fatalf("bad catalog: %+v, %v", catalog, err)
	}
	for name, invalid := range map[string]string{
		"unlisted current": strings.Replace(valid, `"current":`+ref, `"current":{"provider":"fixture","modelId":"b"}`, 1),
		"unknown field":    strings.Replace(valid, `"label":`, `"baseUrl":"http://local","label":`, 1),
		"unsafe label":     strings.Replace(valid, "Fixture A", `Fixture\u001b`, 1),
		"unsupported tier": strings.Replace(valid, `"off"`, `"ultra"`, 1),
		"duplicate tier":   strings.Replace(valid, `"off"`, `"off","off"`, 1),
		"missing reason":   strings.Replace(valid, `"reason":null,`, "", 1),
		"missing flag":     strings.Replace(valid, `"available":true,`, "", 1),
		"duplicate model":  strings.Replace(valid, row, row+","+row, 1),
		"oversized":        strings.Replace(valid, row, strings.Repeat(row+",", ModelCatalogLimit)+row, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeModelCatalog([]byte(invalid), false); err != ErrProtocol {
				t.Fatalf("wanted protocol error, got %v", err)
			}
		})
	}
}

func TestDecodeFilteredModelCatalog(t *testing.T) {
	const noMatches = `{"default":{"provider":"fixture","modelId":"a"},"current":{"provider":"fixture","modelId":"b"},"models":[]}`
	if catalog, err := decodeModelCatalog([]byte(noMatches), true); err != nil || catalog.Current == nil || len(catalog.Models) != 0 {
		t.Fatalf("wanted an empty result with the saved model reference: %+v, %v", catalog, err)
	}
	if _, err := decodeModelCatalog([]byte(noMatches), false); err != ErrProtocol {
		t.Fatalf("unfiltered catalog must include both pins, got %v", err)
	}
}

func TestCatalogDoesNotMutateBareHost(t *testing.T) {
	clientSide, hostSide := net.Pipe()
	defer hostSide.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := hostSide.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client := NewClient("", ClientOptions{
		Capabilities: map[string][]uint64{"modelCatalog": {ModelCatalogVersion}},
		DialUnix:     func(context.Context, string) (net.Conn, error) { return clientSide, nil },
	})
	defer client.Close()
	hostDone := make(chan error, 1)
	go func() {
		r := bufio.NewReader(hostSide)
		line, err := r.ReadBytes('\n')
		if err != nil {
			hostDone <- err
			return
		}
		var req struct {
			ID     string
			Method string
			Params struct{ Capabilities map[string][]uint64 }
		}
		if err = json.Unmarshal(line, &req); err != nil || req.Method != "hello" ||
			len(req.Params.Capabilities) != 2 || len(req.Params.Capabilities["modelCatalog"]) != 1 || req.Params.Capabilities["modelCatalog"][0] != 1 ||
			len(req.Params.Capabilities[storeIdentityCap]) != 1 || req.Params.Capabilities[storeIdentityCap][0] != StoreIdentityVersion {
			hostDone <- fmt.Errorf("incorrect hello: %s", line)
			return
		}
		if _, err = fmt.Fprintf(hostSide, `{"type":"response","id":%q,"method":"hello","ok":true,"result":{"version":1,"epoch":"bare","capabilities":{}}}`+"\n", req.ID); err != nil {
			hostDone <- err
			return
		}
		line, err = r.ReadBytes('\n')
		if len(line) != 0 || err == nil {
			hostDone <- fmt.Errorf("unexpected bare-host request: %s", line)
			return
		}
		hostDone <- nil
	}()
	if _, err := client.ModelCatalog(ctx, "1"); !IsCode(err, "unsupported_capability") {
		t.Fatalf("bare catalog: %v", err)
	}
	if err := client.ConfigureCatalogThread(ctx, "1", &Model{Provider: "fixture", ModelID: "a"}, "off"); !IsCode(err, "unsupported_capability") {
		t.Fatalf("bare configuration: %v", err)
	}
	if _, err := client.NewCatalogThread(ctx, "/workspace", Model{Provider: "fixture", ModelID: "a"}, "off"); !IsCode(err, "unsupported_capability") {
		t.Fatalf("bare catalog creation: %v", err)
	}
	client.Close()
	if err := <-hostDone; err != nil {
		t.Fatal(err)
	}
}
