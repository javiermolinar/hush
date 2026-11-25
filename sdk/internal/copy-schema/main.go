package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
)

func main() {
	copySchema("../protocol/v1/core.schema.json", "internal/core.schema.json")
	copySchema("../protocol/v1/intro.schema.json", "internal/intro.schema.json")
	copySchema("../protocol/v1/thread-directory.schema.json", "internal/thread-directory.schema.json")
	copySchema("../protocol/v1/images.schema.json", "internal/images.schema.json")
}

func copySchema(source, target string) {
	data, err := os.ReadFile(source)
	if err != nil {
		log.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		log.Fatal(err)
	}
	compact.WriteByte('\n')
	if err := os.WriteFile(target, compact.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}
