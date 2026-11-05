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

// Command convert-v1alpha1 converts v1alpha1 FeedGroup manifests, read from
// stdin, to v1beta1 manifests on stdout. Notes on conversions worth checking
// go to stderr. See docs/schema.md, "Upgrading from v1alpha1".
//
//	kubectl get feedgroups -A -o yaml | go run ./cmd/convert-v1alpha1 > feedgroups-v1beta1.yaml
package main

import (
	"fmt"
	"os"

	"github.com/maverickd650-labs/rss2discord/internal/migrate"
)

func main() {
	err := migrate.Convert(os.Stdin, os.Stdout, func(msg string) {
		fmt.Fprintln(os.Stderr, "note:", msg)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
