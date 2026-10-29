package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

const (
	imageStyleField    = "imageStyle"
	retriesField       = "retries"
	catchUpLimitField  = "catchUpLimit"
	intervalField      = "interval"
	retryIntervalField = "retryInterval"
	avatarURLField     = "avatarURL"
	embedField         = "embed"
	feedEmbedField     = "feed.embed"
	forumThreadIDField = "feed.forumThreadID"
)

// Use Kubernetes' OpenAPI and CEL validators against the generated schema.
// This checks admission rules without starting an API server or etcd.
func TestFeedGroupValidation_NewBounds(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "rss2discord.maverickd650.dev_feedgroups.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	var specSchema apiextensions.JSONSchemaProps
	found := false
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1alpha1" {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				t.Fatal("v1alpha1 schema is missing")
			}
			spec := version.Schema.OpenAPIV3Schema.Properties["spec"]
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&spec, &specSchema, nil); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("v1alpha1 schema version is missing")
	}
	openAPI, _, err := validation.NewSchemaValidator(&specSchema)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&specSchema)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if celValidator == nil {
		t.Fatal("schema has no CEL validation rules")
	}
	for _, tc := range []struct {
		name, field string
		value       any
		invalid     bool
	}{
		{name: "retries below minimum", field: retriesField, value: int64(-1), invalid: true},
		{name: "retries minimum", field: retriesField, value: int64(0)},
		{name: "retries maximum", field: retriesField, value: int64(100)},
		{name: "retries above maximum", field: retriesField, value: int64(101), invalid: true},
		{name: "catchUpLimit below minimum", field: catchUpLimitField, value: int64(-1), invalid: true},
		{name: "catchUpLimit minimum", field: catchUpLimitField, value: int64(0)},
		{name: "catchUpLimit maximum", field: catchUpLimitField, value: int64(100)},
		{name: "catchUpLimit above maximum", field: catchUpLimitField, value: int64(101), invalid: true},
		{name: "interval just below minimum", field: intervalField, value: "59.999999999s", invalid: true},
		{name: "interval minimum", field: intervalField, value: "1m"},
		{name: "interval equivalent seconds", field: intervalField, value: "60s"},
		{name: "interval compound minimum", field: intervalField, value: "59s1000ms"},
		{name: "interval fractional minimum", field: intervalField, value: "0.5m30s"},
		{name: "interval zero", field: intervalField, value: "0s", invalid: true},
		{name: "interval overflow", field: intervalField, value: "9999999999999h", invalid: true},
		{name: "microsecond symbol rejected even above minimum", field: intervalField, value: "1m1µs", invalid: true},
		{name: "ascii microseconds", field: intervalField, value: "1m1us"},
		{name: "retry just below minimum", field: retryIntervalField, value: "9.999999999s", invalid: true},
		{name: "retry minimum", field: retryIntervalField, value: "10s"},
		{name: "retry compound minimum", field: retryIntervalField, value: "9s1000ms"},
		{name: "retry fractional minimum", field: retryIntervalField, value: "0.1m4s"},
		{name: "retry overflow", field: retryIntervalField, value: "9999999999999h", invalid: true},
		{name: "retry microsecond symbol", field: retryIntervalField, value: "10s1µs", invalid: true},
		{name: "https avatar", field: avatarURLField, value: "https://example.com/a.png"},
		{name: "http avatar", field: avatarURLField, value: "http://example.com/a.png"},
		{name: "relative avatar", field: avatarURLField, value: "//example.com/a.png", invalid: true},
		{name: "data avatar", field: avatarURLField, value: "data:image/png;base64,a", invalid: true},
		{name: "ftp avatar", field: avatarURLField, value: "ftp://example.com/a.png", invalid: true},
		{name: "group large image", field: embedField, value: map[string]any{imageStyleField: "image"}},
		{name: "group thumbnail", field: embedField, value: map[string]any{imageStyleField: "thumbnail"}},
		{name: "group invalid style", field: embedField, value: map[string]any{imageStyleField: "banner"}, invalid: true},
		{name: "feed large image", field: feedEmbedField, value: map[string]any{imageStyleField: "image"}},
		{name: "feed thumbnail", field: feedEmbedField, value: map[string]any{imageStyleField: "thumbnail"}},
		{name: "feed invalid style", field: feedEmbedField, value: map[string]any{imageStyleField: "IMAGE"}, invalid: true},
		{name: "single digit thread", field: forumThreadIDField, value: "0"},
		{name: "maximum length thread", field: forumThreadIDField, value: strings.Repeat("9", 32)},
		{name: "oversized thread", field: forumThreadIDField, value: strings.Repeat("9", 33), invalid: true},
		{name: "signed thread", field: forumThreadIDField, value: "-123", invalid: true},
		{name: "thread with whitespace", field: forumThreadIDField, value: "123 ", invalid: true},
		{name: "non ascii thread digits", field: forumThreadIDField, value: "１２３", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feed := map[string]any{"rssUrl": "https://example.com/feed.xml"}
			spec := map[string]any{
				"discordWebhookSecretRef": map[string]any{"name": "webhook", "key": "url"},
				"feeds":                   []any{feed},
			}
			if name, ok := strings.CutPrefix(tc.field, "feed."); ok {
				feed[name] = tc.value
			} else {
				spec[tc.field] = tc.value
			}
			errs := validation.ValidateCustomResource(field.NewPath("spec"), spec, openAPI)
			celErrors, _ := celValidator.Validate(t.Context(), field.NewPath("spec"), structural, spec, nil, celconfig.RuntimeCELCostBudget)
			errs = append(errs, celErrors...)
			if (len(errs) > 0) != tc.invalid {
				t.Fatalf("validation errors=%v, want invalid=%t", errs, tc.invalid)
			}
			if tc.invalid {
				wantField := strings.TrimPrefix(tc.field, "feed.")
				if !strings.Contains(errs.ToAggregate().Error(), wantField) {
					t.Fatalf("expected rejection on %s, got %v", wantField, errs)
				}
			}
		})
	}
}
