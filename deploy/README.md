# deploy/

Deployment assets (Helm chart under `deploy/charts/<name>/`, example manifests).
Empty for now; the CI `helm` job runs `helm lint` and `kubeconform` once a
`Chart.yaml` exists here.
