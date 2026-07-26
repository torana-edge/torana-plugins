package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 4 && len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: bundle_digest <plugin.json> <plugin.wasm> <schema.json> [agent.json]")
		os.Exit(2)
	}
	h := sha256.New()
	paths := os.Args[1:]
	for _, path := range paths {
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
