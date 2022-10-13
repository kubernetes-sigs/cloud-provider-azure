{{/*
Determine the version of cloud-provider-azure to use for cloud-controller-manager
*/}}
{{- define "image.cloudControllerManager" -}}
    {{- if hasKey .Values.cloudControllerManager "imageTag" -}}
        {{- printf "%s/%s:%s" .Values.cloudControllerManager.imageRepository .Values.cloudControllerManager.imageName .Values.cloudControllerManager.imageTag -}}
    {{- else if not .cloudProviderAzureVersion }}
        {{- printf "" -}}
    {{- else -}}
        {{- printf "%s/%s:%s" .Values.cloudControllerManager.imageRepository .Values.cloudControllerManager.imageName .cloudProviderAzureVersion -}}
    {{- end -}}
{{- end -}}
{{/*
Determine the version of cloud-provider-azure to use for cloud-node-manager
*/}}
{{- define "image.cloudNodeManager" -}}
    {{- if hasKey .Values.cloudNodeManager "imageTag" -}}
        {{- printf "%s/%s:%s" .Values.cloudNodeManager.imageRepository .Values.cloudNodeManager.imageName .Values.cloudNodeManager.imageTag -}}
    {{- else if not .cloudProviderAzureVersion }}
        {{- printf "" -}}
    {{- else -}}
        {{- printf "%s/%s:%s" .Values.cloudNodeManager.imageRepository .Values.cloudNodeManager.imageName .cloudProviderAzureVersion -}}
    {{- end -}}
{{- end -}}
