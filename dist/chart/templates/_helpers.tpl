{{/*
Expand the name of the chart.
*/}}
{{- define "rss2discord-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "rss2discord-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "rss2discord-operator.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "rss2discord-operator.resourceName" -}}
{{- $fullname := include "rss2discord-operator.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Controller-manager workload name (Deployment, ServiceAccount).
Uses the plain fullname so resources don't carry a verbose
"-controller-manager" suffix; the "control-plane: controller-manager"
label is what selectors actually match on, so names stay short while
selection is unaffected.
*/}}
{{- define "rss2discord-operator.controllerManagerName" -}}
{{- include "rss2discord-operator.fullname" . }}
{{- end }}

{{/*
ServiceAccount name to use.
When enabled, use the controller-manager workload name.
When disabled, use serviceAccount.name if set (use "default" to pick the namespace
default ServiceAccount); otherwise fall back to the controller-manager workload name,
i.e. expect a pre-created ServiceAccount with the name the chart would have used. The
fallback never silently selects the namespace default ServiceAccount, which would grant
the operator's RBAC to every pod in the namespace that runs under it.
*/}}
{{- define "rss2discord-operator.serviceAccountName" -}}
{{- $default := include "rss2discord-operator.controllerManagerName" . }}
{{- if .Values.serviceAccount.enabled }}
{{- $default }}
{{- else }}
{{- .Values.serviceAccount.name | default "" | trim | default $default }}
{{- end }}
{{- end }}

{{/*
Namespaces the manager watches and is granted its RBAC in, comma-separated:
rbac.watchNamespaces if set, else the release namespace when rbac.namespaced,
else empty (every namespace, via a ClusterRole). manager.yaml passes the same
list as --watch-namespaces and rbac/manager-role(binding).yaml render a Role
and RoleBinding per entry, so the watch scope and the grant always match:
narrowing only the watch would leave a stolen token able to read every Secret.
*/}}
{{- define "rss2discord-operator.watchNamespaces" -}}
{{- if .Values.rbac.watchNamespaces }}
{{- .Values.rbac.watchNamespaces | uniq | join "," }}
{{- else if .Values.rbac.namespaced }}
{{- .Release.Namespace }}
{{- end }}
{{- end }}
