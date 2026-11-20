---
name: vps-prod-deploy
description: "Use when deploying stacks to a multi-tenant prod VPS."
---

# Production VPS Deployment (general)

Workflow deploy stack production ke VPS multi-tenant (service
tenant lain hidup dan tidak boleh terganggu). Stack-agnostic.
Edge aaPanel → skill `aapanel-edge-deploy`. Provisioning Zitadel
→ skill `zitadel`. Topologi/deploy MokiBox → skill
`mokibox-go-shared` `references/deployment-topology.md`.

## Pre-flight (quick-verify, BUKAN audit ulang)

Kalau sudah ada audit/dokumen VPS, cukup verify cepat sebelum mulai:

- `docker ps` — jumlah + nama container tenant sesuai audit.
- `free -h` — headroom memori + kebijakan swap masih sesuai.
- `ss -tlnp | grep -E ':80|:443'` — pemilik edge port tidak berubah.
- Direktori repo target ada/belum, di branch yang diharapkan.
- Guard sisa deploy gagal sebelumnya: volume/network
  (`docker volume ls | grep <project>`), port loopback tujuan
  bebas (`ss -tlnp | grep 818`).
- `df -h /` untuk disk.

## Multi-tenant safety (aturan keras)

- Snapshot `docker ps --format '{{.Names}}\t{{.Status}}'` SEBELUM
  dan SESUDAH seluruh operasi — diff untuk container tenant lain
  harus kosong.
- Jangan pernah: publish port yang dipakai host/tenant lain,
  restart/stop container tenant lain, edit vhost domain lain,
  ubah state global host (swap, sysctl, dist-upgrade).
- Anggaran memori: hitung total stack baru vs available; kalau
  sempit, turunkan `mem_limit` per-service (worker duluan) atau
  concurrency — JANGAN tambah swap di host multi-tenant.
- `docker stats` minggu pertama; OOM-kill di log = turunkan
  limit/concurrency service pelaku, jangan naikkan anggaran host.

## Secrets hygiene

- Generate FRESH on-VPS (`openssl rand -hex 16/24`) — JANGAN copy
  secret dev ke prod.
- Nilai secret TIDAK boleh transit context agent (transcript/chat):
  - Transfer user-run (paling aman): user menjalankan SSH one-liner
    yang pipe langsung laptop→VPS, mis.:
    `ssh user@vps "sudo bash -c 'grep -E \"^R2_\" /dev/stdin >> /www/app/.env'" < <(grep -E '^R2_' ~/repo/.env)`
  - Transfer agent: script yang generate/read nilai ON-VPS; output
    hanya key-names + shape (panjang/charset), never nilai.
- Verify by shape, bukan isi: `grep -cE '^R2_[A-Z_]+=.+$' .env`,
  `awk -F= '/^R2_SECRET/{print length($2)}' .env`.
- Credential yang sempat muncul di output/transcript → flag ROTASI
  segera di laporan akhir; jangan didiamkan.
- Jangan bangun command yang embed password (echo/sed berisi
  credential) — pakai `sudo -S` dengan stdin dari file 600,
  askpass script, atau sudoers NOPASSWD untuk command spesifik.
  Embed password di command line = bocor ke history + transcript.

## Env-cache trap (penyebab kebingungan #1)

Container menangkap env SAAT dibuat. Edit `.env` TIDAK mengubah
container yang sudah jalan — wajib:
`docker compose -f docker-compose.yml up -d --force-recreate <svc>`.
Symptom klasik: nilai baru di .env sudah benar, tapi app tetap
403 / SignatureDoesNotMatch / auth gagal — container masih
memegang secret lama. Cek env actual container:
`docker inspect <c> --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c <KEY>`
(distroless container tidak bisa `docker exec printenv`).

## Remote-ops tooling patterns

- **MCP/SSH run_command + heredoc multi-line = timeout/hang.**
  Untuk script >30 baris: commit ke repo → `git pull` di VPS
  (artifact reproducible + versioned — terbaik), atau sftp_upload,
  atau base64-chunks untuk patch kecil.
- **Tugas lama (docker build multi-stage, integration test) →
  nohup background**: `nohup bash script.sh > /tmp/deploy.log 2>&1
  & echo $!` lalu poll log — jangan sandera sesi SSH/MCP yang
  timeout ~300s.
- **Provider mirror trap**: VPS Tencent Cloud memakai
  `mirror.cloud.tencent.com` yang tidak resolvable dari luar VPC
  → semua `apt install` gagal. Swap `sources.list` ke
  `archive.ubuntu.com`, install, restore backup.
- **CLI tool hilang di host VPS**: jq → static binary dari GitHub
  releases via curl (`jq-linux-amd64` → `chmod +x
  /usr/local/bin/jq`) — lebih cepat daripada perbaiki apt.
- SSH non-interaktif dari laptop: askpass script
  (`SSH_ASKPASS=<script> SSH_ASKPASS_REQUIRE=force setsid ssh ...`)
  yang baca password dari file config — password tidak lewat
  command line.

## Urutan start (dependency HTTPS issuer)

App yang memvalidasi TLS ke issuer URL https miliknya sendiri
(mis. OIDC verifier) akan crash-loop sampai ada cert
publicly-trusted untuk domain itu. Urutan wajib:

1. DNS A record live (DNS-only) →
2. vhost + placeholder cert (self-signed) →
3. LE cert via acme.sh → reload nginx →
4. baru recreate container app pemanggil verifier.

Crash-loop app selama fase 1-3 = expected state, jangan didebug
sebagai bug app. Verifier dengan retry/backoff akan recover
sendiri setelah cert valid; yang butuh restart eksplisit hanyalah
container yang dibuat SEBELUM cert ada (recreate, bukan restart
saja, kalau env-nya juga berubah).

## Integration test

- Script yang `docker exec` / baca docker volume WAJIB dijalankan
  ON THE VPS (SSH session), bukan dari laptop — semua coupled ke
  Docker host yang mengeksekusi.
- Script env-driven (baca `.env`: API_BASE_URL, dsb.) otomatis
  pakai domain prod — pastikan tidak ada hostname hardcoded.
- **Rerun-ability: harus PASS 2× back-to-back** sebelum dinyatakan
  selesai — script state-mutating wajib reset state miliknya di
  awal script, bukan mengasumsikan environment fresh.

## Verification ladder (endpoint asli, bukan efek samping)

Berurutan, jangan lompat:

1. `curl -i https://domain/healthz` → 200.
2. OIDC discovery → 200 + field `issuer` = URL prod PERSIS
   (issuer salah = instance salah/env salah).
3. Endpoint authed tanpa token → 401 ENVELOPE app (bukan html
   default page nginx).
4. Headless login user test → JWT → call API authed → 200 (+ row
   user auto-created bila lazy-provision).
5. Event eksternal REAL (webhook dari provider asli, dsb.) →
   side-effect benar di DB — self-simulated curl bukan bukti
   koneksi provider↔app.
6. CORS/origin probe dengan ORIGIN PROD (https!) — origin http
   yang lolos di dev BUKAN bukti origin https prod jalan; probe
   object store dengan header `Origin: https://prod-domain`.
7. Log worker/ticker jalan tanpa permission error (bukti grant
   DB benar), `docker logs --since 10m <worker>`.
8. `docker stats` — semua service dalam anggaran memori.

## Deviasi & dokumentasi

- Semua deviasi dari planning doc (path beda, endpoint beda,
  branch beda) dicatat di HANDOFF/commit message — jangan silent.
- Perubahan prod (compose overlay, dsb.) di branch deploy
  terpisah sesuai konvensi repo, bukan main; merge = keputusan
  user, jangan self-merge.
- Post-deploy checklist: cron renewal cert ada (`crontab -l |
  grep acme`), HANDOFF updated (domain, IP, tanggal, gotcha),
  password/token yang ter-selip di transcript dirotasi.
