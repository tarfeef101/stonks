# Optional: daily Postgres backup CronJob + PVC — mirrors the app repo's
# deploy/ops/pg-backup-cronjob.yaml verbatim (D-037: ops topology stays
# operator-owned). Disabled by default; enable with
# -var pg_backup_enabled=true, or `kubectl apply -f` the app repo manifest
# and drop this file.
#
# The PVC lives on the cluster (Longhorn) — it does NOT survive total
# cluster loss. If you want an off-cluster copy, add your own transport
# (the dumps are portable plain-SQL gzips, e.g. an `aws s3 cp` step).

resource "kubernetes_manifest" "backups_pvc" {
  count = var.pg_backup_enabled ? 1 : 0

  # PVC binding is async (Longhorn provisioner) — don't block the apply.
  # The `wait { }` block is opt-in in the 2.x and 3.x providers; omitting
  # it keeps the apply non-blocking.
  manifest = <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: stonks-backups
  namespace: ${var.namespace}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${var.pg_backup_storage_class}
  resources:
    requests:
      storage: 10Gi
YAML
}

resource "kubernetes_manifest" "pg_backup" {
  count = var.pg_backup_enabled ? 1 : 0

  depends_on = [kubernetes_manifest.backups_pvc]

  manifest = <<YAML
apiVersion: batch/v1
kind: CronJob
metadata:
  name: stonks-pg-backup
  namespace: ${var.namespace}
  labels:
    app.kubernetes.io/name: stonks
    app.kubernetes.io/component: ops
spec:
  # 03:00 UTC daily (CronJob schedules run in the apiserver's local time;
  # k3s defaults to UTC). RPO = 24h.
  schedule: "0 3 * * *"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: pg-dump
              # Keep in sync with `postgres.image.tag` in the release values.
              image: postgres:${var.postgres_image_tag}
              imagePullPolicy: IfNotPresent
              # Runs as root on purpose: a custom command bypasses the
              # postgres image's entrypoint (which would drop privileges),
              # and a fresh Longhorn PVC is root-owned. One-shot, internal,
              # holds only a psql client + the dump output.
              command: ["/bin/sh", "-c"]
              args:
                - |
                  set -eu
                  d=$(date -u +%Y%m%dT%H%M%SZ)
                  tmp="/backups/.dump-$d.sql.part"
                  out="/backups/stonks-$d.sql.gz"
                  pg_dump -h stonks-postgres -p 5432 -U "$PGUSER" -d "$PGDATABASE" \
                    --format=plain --no-owner --no-privileges > "$tmp"
                  gzip -c "$tmp" > "$out"
                  rm -f "$tmp"
                  echo "backup written: $out ($(du -h "$out" | cut -f1))"
                  # rolling retention: remove everything past the 14 newest
                  for f in $(ls -1t /backups/stonks-*.sql.gz 2>/dev/null | tail -n +15); do
                    rm -f -- "$f"
                  done
              env:
                - name: PGUSER
                  valueFrom:
                    secretKeyRef:
                      name: ${var.release_name}-secrets
                      key: POSTGRES_USER
                - name: PGDATABASE
                  valueFrom:
                    secretKeyRef:
                      name: ${var.release_name}-secrets
                      key: POSTGRES_DB
                - name: PGPASSWORD
                  valueFrom:
                    secretKeyRef:
                      name: ${var.release_name}-secrets
                      key: POSTGRES_PASSWORD
              resources:
                requests:
                  cpu: 100m
                  memory: 256Mi
                limits:
                  cpu: 500m
                  memory: 512Mi
              volumeMounts:
                - name: backups
                  mountPath: /backups
          volumes:
            - name: backups
              persistentVolumeClaim:
                claimName: stonks-backups
YAML
}
