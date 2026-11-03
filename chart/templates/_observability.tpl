{{/* Standard OTLP settings; authentication comes from Secret refs. */}}
{{- define "buildkit-service.telemetryEnv" -}}
{{- $root := .root -}}
{{- if $root.Values.observability.tracing.enabled }}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_UID
  valueFrom:
    fieldRef:
      fieldPath: metadata.uid
- name: OTEL_SERVICE_NAME
  value: {{ .service | quote }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: "k8s.namespace.name=$(POD_NAMESPACE),k8s.pod.name=$(POD_NAME),service.instance.id=$(POD_UID)"
- name: OTEL_TRACES_EXPORTER
  value: otlp
- name: OTEL_METRICS_EXPORTER
  value: none
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ required "observability.tracing.endpoint is required" $root.Values.observability.tracing.endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: {{ $root.Values.observability.tracing.protocol | quote }}
- name: OTEL_TRACES_SAMPLER
  value: {{ $root.Values.observability.tracing.sampler | quote }}
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ $root.Values.observability.tracing.samplerArg | quote }}
{{- with $root.Values.observability.tracing.headersSecretName }}
- name: OTEL_EXPORTER_OTLP_HEADERS
  valueFrom:
    secretKeyRef:
      name: {{ . }}
      key: headers
{{- end }}
{{- if $root.Values.observability.tracing.caSecretName }}
- name: OTEL_EXPORTER_OTLP_CERTIFICATE
  value: /var/run/secrets/otel/ca.crt
{{- end }}
{{- end }}
{{- end -}}
