# Deploying Glovebox with Helm (Two RSS Sources)

*2026-03-31T07:16:58Z by Showboat 0.6.1*
<!-- showboat-id: b35a8988-e932-41a1-bd75-3129ce3ad5c6 -->

This demo shows how to use the Helm chart to deploy glovebox with the RSS connector configured to poll two different feeds. We use `helm template` to render the manifests without requiring a live cluster.

First, inspect the chart structure:

```bash
ls charts/glovebox/templates/
```

```output
_helpers.tpl
configmap.yaml
connector-configmap.yaml
connector-deployment.yaml
connector-pvc.yaml
connector-service.yaml
deployment.yaml
networkpolicy.yaml
pvc.yaml
service.yaml
servicemonitor.yaml
```

Create a custom values file that enables the RSS connector with two feeds:

```bash
cat > /tmp/demo-values.yaml << 'VALUES'
image:
  repository: ghcr.io/leftathome/glovebox
  tag: "0.2.0"

config:
  agentAllowlist:
    - media
    - news

connectors:
  rss:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-rss
      tag: "0.2.0"
    config:
      rules:
        - match: "feed:engadget"
          destination: media
          tags:
            source: engadget
            category: tech
        - match: "feed:ars-technica"
          destination: news
          tags:
            source: ars
            category: tech
        - match: "*"
          destination: media
      feeds:
        - name: engadget
          url: "https://www.engadget.com/rss.xml"
        - name: ars-technica
          url: "https://feeds.arstechnica.com/arstechnica/index"
      fetch_links: false
      link_policy:
        default: safe
    resources:
      requests:
        cpu: 100m
        memory: 64Mi
      limits:
        cpu: 300m
        memory: 128Mi

metrics:
  annotations: true
  serviceMonitor:
    enabled: true
    labels:
      release: kube-prometheus
VALUES
echo "custom values written"
```

```output
custom values written
```

Render the manifests to see what would be deployed:

```bash
helm template glovebox charts/glovebox/ -f /tmp/demo-values.yaml 2>&1 | grep 'kind:\|name:\|prometheus.io' | head -30
```

```output
kind: NetworkPolicy
  name: glovebox-glovebox
kind: ConfigMap
  name: glovebox-glovebox-config
kind: ConfigMap
  name: glovebox-glovebox-rss-config
kind: PersistentVolumeClaim
  name: glovebox-glovebox-rss-state
kind: PersistentVolumeClaim
  name: glovebox-glovebox-agents
kind: PersistentVolumeClaim
  name: glovebox-glovebox-audit
kind: PersistentVolumeClaim
  name: glovebox-glovebox-failed
kind: PersistentVolumeClaim
  name: glovebox-glovebox-quarantine
kind: PersistentVolumeClaim
  name: glovebox-glovebox-shared
kind: PersistentVolumeClaim
  name: glovebox-glovebox-staging
kind: Service
  name: glovebox-glovebox-rss
      name: health
kind: Service
  name: glovebox-glovebox-metrics
      name: metrics
kind: Deployment
  name: glovebox-glovebox-rss
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
```

Count the resources that would be created:

```bash
helm template glovebox charts/glovebox/ -f /tmp/demo-values.yaml 2>&1 | grep '^kind:' | sort | uniq -c | sort -rn
```

```output
      7 kind: PersistentVolumeClaim
      2 kind: ServiceMonitor
      2 kind: Service
      2 kind: Deployment
      2 kind: ConfigMap
      1 kind: NetworkPolicy
```

Inspect the rendered RSS connector deployment:

```bash
helm template glovebox charts/glovebox/ -f /tmp/demo-values.yaml 2>&1 | sed -n '/Deployment/,/^---/p' | grep -A100 'glovebox-rss' | head -40
```

```output
  name: glovebox-glovebox-rss
  labels:
    app: glovebox-connector
    connector: rss
    release: glovebox
spec:
  replicas: 1
  selector:
    matchLabels:
      app: glovebox-connector
      connector: rss
      release: glovebox
  template:
    metadata:
      labels:
        app: glovebox-connector
        connector: rss
        release: glovebox
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
        prometheus.io/path: "/metrics"
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
      containers:
        - name: rss
          image: "ghcr.io/leftathome/glovebox-rss:0.2.0"
          imagePullPolicy: IfNotPresent
          env:
            - name: GLOVEBOX_STAGING_DIR
              value: /data/glovebox/staging
            - name: GLOVEBOX_STATE_DIR
              value: /state
            - name: GLOVEBOX_CONNECTOR_CONFIG
              value: /etc/connector/config.json
          ports:
            - name: health
              containerPort: 8080
```

To deploy to a real cluster, you would run:

```
helm install glovebox charts/glovebox/ -f values-production.yaml -n glovebox --create-namespace
```

Clean up:

```bash
rm /tmp/demo-values.yaml && echo 'cleaned up'
```

```output
cleaned up
```
