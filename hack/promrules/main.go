/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command promrules extracts the .spec of a rendered PrometheusRule custom
// resource (read from stdin) and prints it as a plain Prometheus rules file
// (stdout), since `promtool check/test rules` only understands the latter
// and has no notion of the CRD wrapper around it.
package main

import (
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v4"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "promrules:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	input, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	var resource struct {
		Spec yaml.Node `yaml:"spec"`
	}
	if err := yaml.Unmarshal(input, &resource); err != nil {
		return fmt.Errorf("parsing PrometheusRule YAML: %w", err)
	}
	if resource.Spec.IsZero() {
		return fmt.Errorf("input has no top-level 'spec' key -- is this a rendered PrometheusRule?")
	}

	encoder := yaml.NewEncoder(out)
	if err := encoder.Encode(&resource.Spec); err != nil {
		return fmt.Errorf("writing rules file: %w", err)
	}
	return encoder.Close()
}
