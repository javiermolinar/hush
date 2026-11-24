// Package authproviders lists the built-in Pi OAuth flows reviewed for local Hush sign-in.
package authproviders

import (
	_ "embed"
	"encoding/json"
	"slices"
)

const Default = "openai-codex"

//go:embed providers.json
var manifest []byte

type Provider struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

var providers = func() []Provider {
	var list []Provider
	if err := json.Unmarshal(manifest, &list); err != nil {
		panic(err)
	}
	return list
}()

func All() []Provider { return slices.Clone(providers) }

func Lookup(id string) (Provider, bool) {
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}
