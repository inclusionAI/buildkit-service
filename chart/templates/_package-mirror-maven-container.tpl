{{/*
Shared Reposilite (Maven) container spec. Used as the sole container of the
standalone maven Deployment. Authored at zero indentation; callers must render
it with `nindent 8` under a `containers:` list.

Reposilite reads two configuration layers:
  - local configuration (hostname/port/database), set through REPOSILITE_OPTS and
    REPOSILITE_LOCAL_* environment variables, so it needs no writable config file
  - shared configuration (repositories/mirrors), linked read-only from a ConfigMap
    with --shared-configuration
The shared file is immutable, so Reposilite never writes it: the ConfigMap
remains the single source of truth and no bootstrap step is required.
*/}}
{{- define "buildkit-service.packageMirror.mavenContainer" -}}
- name: maven
  image: {{ printf "%s:%s" .Values.packageMirror.maven.image.repository .Values.packageMirror.maven.image.tag }}
  imagePullPolicy: {{ .Values.packageMirror.maven.image.pullPolicy }}
  securityContext:
    {{- toYaml .Values.packageMirror.securityContext | nindent 4 }}
  env:
    - name: JAVA_OPTS
      value: {{ .Values.packageMirror.maven.env.javaOpts | quote }}
    # --port is a startup parameter, not a REPOSILITE_LOCAL_* property.
    # --shared-configuration replaces the database-backed shared settings with
    # the linked read-only JSON file.
    - name: REPOSILITE_OPTS
      value: {{ printf "--port=%v --shared-configuration=/etc/package-mirror/configuration.shared.json" (.Values.packageMirror.maven.port | int) | quote }}
  ports:
    - name: {{ .Values.packageMirror.maven.portName }}
      containerPort: {{ .Values.packageMirror.maven.port }}
      protocol: {{ .Values.packageMirror.maven.protocol }}
  startupProbe:
    tcpSocket:
      port: {{ .Values.packageMirror.maven.portName }}
    failureThreshold: {{ .Values.packageMirror.maven.startupProbe.failureThreshold }}
    initialDelaySeconds: {{ .Values.packageMirror.maven.startupProbe.initialDelaySeconds }}
    periodSeconds: {{ .Values.packageMirror.maven.startupProbe.periodSeconds }}
    timeoutSeconds: {{ .Values.packageMirror.maven.startupProbe.timeoutSeconds }}
  livenessProbe:
    tcpSocket:
      port: {{ .Values.packageMirror.maven.portName }}
    initialDelaySeconds: {{ .Values.packageMirror.maven.livenessProbe.initialDelaySeconds }}
    periodSeconds: {{ .Values.packageMirror.maven.livenessProbe.periodSeconds }}
    timeoutSeconds: {{ .Values.packageMirror.maven.livenessProbe.timeoutSeconds }}
    failureThreshold: {{ .Values.packageMirror.maven.livenessProbe.failureThreshold }}
    successThreshold: {{ .Values.packageMirror.maven.livenessProbe.successThreshold }}
  readinessProbe:
    tcpSocket:
      port: {{ .Values.packageMirror.maven.portName }}
    initialDelaySeconds: {{ .Values.packageMirror.maven.readinessProbe.initialDelaySeconds }}
    periodSeconds: {{ .Values.packageMirror.maven.readinessProbe.periodSeconds }}
    timeoutSeconds: {{ .Values.packageMirror.maven.readinessProbe.timeoutSeconds }}
    failureThreshold: {{ .Values.packageMirror.maven.readinessProbe.failureThreshold }}
    successThreshold: {{ .Values.packageMirror.maven.readinessProbe.successThreshold }}
  resources:
    {{- toYaml .Values.packageMirror.maven.resources | nindent 4 }}
  volumeMounts:
    - name: {{ include "buildkit-service.packageMirror.mavenDataVolumeName" . }}
      mountPath: /app/data
    - name: {{ include "buildkit-service.packageMirror.mavenConfigVolumeName" . }}
      mountPath: /etc/package-mirror/configuration.shared.json
      subPath: configuration.shared.json
      readOnly: true
{{- end -}}