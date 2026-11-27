// Command configlint validates YAML and JSON configuration files that the
// repository depends on (CI workflows, renovate.json, config.yaml). It is the
// verification helper behind the "config file validation" gate check and uses
// only dependencies already present in go.mod, so the gate contract
// ("npm dependencies are the only downloads") is preserved.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"go.yaml.in/yaml/v4"
)

func validate(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	switch {
	case hasSuffix(path, ".yml"), hasSuffix(path, ".yaml"):
		var out any
		return yaml.Unmarshal(data, &out)
	case hasSuffix(path, ".json"):
		var out any
		return json.Unmarshal(data, &out)
	default:
		return fmt.Errorf("unsupported config file extension: %s", path)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: configlint <file> [<file> ...]")
		os.Exit(2)
	}
	failed := false
	for _, path := range os.Args[1:] {
		if err := validate(path); err != nil {
			fmt.Fprintf(os.Stderr, "configlint: %s: %v\n", path, err)
			failed = true
		} else {
			fmt.Printf("configlint: %s OK\n", path)
		}
	}
	if failed {
		os.Exit(1)
	}
}
