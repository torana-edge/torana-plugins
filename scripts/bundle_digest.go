package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: bundle_digest <plugin.json> <plugin.wasm> <schema.json>")
		os.Exit(2)
	}
	h := sha256.New()
	for _, path := range os.Args[1:] {
		part, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(part)
	}
	fmt.Printf("sha256:%x\n", h.Sum(nil))
}
