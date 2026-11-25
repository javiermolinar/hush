//go:build hush_release

package backendstart

import _ "embed"

//go:embed release-assets/hush-backend.mjs
var releaseBackend []byte

//go:embed release-assets/hush-backend.mjs.wasm-pack
var releaseWasmPack []byte
