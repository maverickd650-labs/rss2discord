package controller

import (
	"strings"
	"testing"
	"text/template"
	"text/template/parse"
)

func TestRejectUnboundedConstructs_UnparsedTemplateIsAccepted(t *testing.T) {
	// A template that was never parsed has a nil Tree and nothing to reject.
	tmpl := template.New("unparsed")
	if tmpl.Tree != nil {
		t.Fatal("precondition: expected a nil Tree")
	}
	if err := rejectUnboundedConstructs(tmpl); err != nil {
		t.Fatalf("rejectUnboundedConstructs = %v, want nil", err)
	}
}

func TestRejectUnboundedConstructs_RejectsDefinedTemplates(t *testing.T) {
	tmpl, err := template.New("main").Parse(`{{define "x"}}x{{end}}main`)
	if err != nil {
		t.Fatal(err)
	}
	err = rejectUnboundedConstructs(tmpl)
	if err == nil || !strings.Contains(err.Error(), "{{define}} is not supported") {
		t.Fatalf("rejectUnboundedConstructs = %v, want the define rejection", err)
	}
}

// The parser never produces a nil *parse.PipeNode (every action and branch
// has a pipeline), but checkNode guards against one like it does for a nil
// *parse.ListNode, which an {{if}} without {{else}} does produce.
func TestCheckNode_NilNodesAreAccepted(t *testing.T) {
	for name, node := range map[string]parse.Node{
		"nil pipe": (*parse.PipeNode)(nil),
		"nil list": (*parse.ListNode)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkNode(node); err != nil {
				t.Fatalf("checkNode = %v, want nil", err)
			}
		})
	}
}
