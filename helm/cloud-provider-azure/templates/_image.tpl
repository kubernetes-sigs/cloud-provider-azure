{{/*
Determine the default image repository based on Kubernetes version.
*/}}
{{- define "image.repo" -}}
    {{- $root := (index . 0) }}
    {{- $repoOverride := (index . 1) }}
    {{- if $repoOverride -}}
        {{ $repoOverride }}
    {{- else if semverCompare "< 1.34.0-0" $root.Capabilities.KubeVersion.Version -}}
        {{- /* Prior to v1.34.0, images are published here: */ -}}
        mcr.microsoft.com/oss/kubernetes
    {{- else -}}
        {{- /* Starting with v1.34.0, images are published here: */ -}}
        mcr.microsoft.com/oss/v2/kubernetes
    {{- end -}}
{{- end -}}
{{/*
Determine the version of cloud-provider-azure to use for cloud-controller-manager
*/}}
{{- define "image.cloudControllerManager" -}}
    {{- $repo := include "image.repo" (list . .Values.cloudControllerManager.imageRepository) }}
    {{- if hasKey .Values.cloudControllerManager "imageTag" -}}
        {{- printf "%s/%s:%s" $repo .Values.cloudControllerManager.imageName .Values.cloudControllerManager.imageTag -}}
    {{- else if not .cloudProviderAzureVersion }}
        {{- printf "" -}}
    {{- else -}}
        {{- printf "%s/%s:%s" $repo .Values.cloudControllerManager.imageName .cloudProviderAzureVersion -}}
    {{- end -}}
{{- end -}}
{{/*
Determine the version of cloud-provider-azure to use for cloud-node-manager
*/}}
{{- define "image.cloudNodeManager" -}}
    {{- $repo := include "image.repo" (list . .Values.cloudNodeManager.imageRepository) }}
    {{- if hasKey .Values.cloudNodeManager "imageTag" -}}
        {{- printf "%s/%s:%s" $repo .Values.cloudNodeManager.imageName .Values.cloudNodeManager.imageTag -}}
    {{- else if not .cloudProviderAzureVersion }}
        {{- printf "" -}}
    {{- else -}}
        {{- printf "%s/%s:%s" $repo .Values.cloudNodeManager.imageName .cloudProviderAzureVersion -}}
    {{- end -}}
{{- end -}}
