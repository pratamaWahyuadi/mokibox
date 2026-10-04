# MokiBox Deployment Topology

Detail kenapa MokiBox backend dan Zitadel identity provider
dideploy sebagai 2 compose project terpisah, bukan satu
compose file. Kalau ada pertanyaan "kenapa gak satu compose"
atau "container apa yang harus jalan", baca dulu ini.

## TL;DR

```
/home/pratama/
├── MokiBox/                  # repo backend (Go)
│   ├── docker-compose.yml    → 5 services: postgres, redis,
│   │                           api-gateway, transcoder-worker,
│   │                           nginx
│   ├── Makefile              → make up / down / up-all / up-zitadel ...
│   └── ...
│
└── zitadel-compose/          # Zitadel stack (gitignored in MokiBox)
    └── docker-compose.yml    → 4 services: Traefik proxy,
                                 zitadel-api, zitadel-login,
                                 zitadel-postgres
```

Dua compose project di-host yang sama. `api-gateway` di MokiBox
call Zitadel via `ZITADEL_ISSUER_URL` (HTTP URL yang Traefik
di zitadel-compose publish ke host, dev: `http://localhost:8080`).
Tidak ada `zitadel` service di MokiBox compose, dan tidak ada
shared docker network di antara keduanya.

## Kenapa pisah (history)

LLD v1 (Planning/LLD_PLAN.md) awalnya menulis "satu docker-compose"
mengikuti NFR-11 "satu VPS". Fase 0 attempt:

- `tiktok-zitadel` (v2.54.0) sebagai single service di
  `MokiBox/docker-compose.yml`.
- Env `ZITADEL_EXTERNALDOMAIN=https://auth.example.com` (placeholder).
- Postgres share dengan app MokiBox, database `zitadel`.

**Result: `tiktok-zitadel` restart-looped indefinitely.**

Root cause (3 issues bareng):

1. **Zitadel v3+ arsitektur multi-service.** Upstream Zitadel
   compose layout v3+ adalah:
   ```
   zitadel-proxy       (Traefik)
   zitadel-zitadel-api
   zitadel-zitadel-login
   zitadel-postgres
   ```
   Single container `zitadel` di v2 udah deprecated. Attempt
   untuk squash jadi satu service `zitadel: v2.54.0` di
   MokiBox compose gagal health check.

2. **Placeholder `auth.example.com` gak resolve.** Traefik
   routing butuh domain yang real. Tanpa DNS + TLS, container
   Zitadel gagal start.

3. **Database `zitadel` gak ada di Postgres MokiBox.** Migrations
   `001_init.sql` cuma setup `tiktok` DB. Container Zitadel
   crash di startup pas coba konek DB yang gak ada.

Fix (PR #36 `fix/zitadel-separate-deployment`):

- Hapus service `zitadel` dari `MokiBox/docker-compose.yml`.
- Rename container `tiktok-*` → `mokibox-*` (compose project
  `tiktok-backend` → `mokibox`) untuk menghindari clash
  dengan orphan containers.
- Nginx MokiBox stop reverse-proxy Zitadel. Zitadel di-proxy
  oleh Traefik sendiri di `zitadel-compose`.
- Makefile tambah targets: `up-zitadel`, `down-zitadel`,
  `up-all`, `down-all`, `logs-zitadel`. Default `up`/`down`
  cuma sentuh MokiBox compose.
- LLD asumsi A11 + PRD NFR-11/FR-AUTH-04 di-rewrite untuk
  reflect separation. NFR-11 spirit ("satu VPS") tetap
  terpenuhi; literal "satu docker-compose" di-drop dengan
  documented DEVIATION.

## Symptom recognition (kalau kembali salah)

| Symptom | Root cause | Fix |
|---|---|---|
| `tiktok-zitadel Restarting (1) X seconds ago` | Zitadel sebagai service di MokiBox compose (sudah deprecated) | Lihat PR #36; Zitadel harus di sibling project |
| `mokibox-nginx Restarting (1) X seconds ago` | Missing `/etc/nginx/certs/live/api.example.com/fullchain.pem` | Mount self-signed dev cert atau certbot; BUKAN masalah Zitadel |
| `zitadel-zitadel-api-1 Restarting` | Biasanya `zitadel-postgres-1` belum healthy atau env `ZITADEL_EXTERNALDOMAIN` invalid | Cek `make logs-zitadel`; biasanya fix env di `zitadel-compose/.env` |
| `mokibox-api-gateway 401 UNAUTHORIZED "invalid access token"` padahal token dari Zitadel valid | `ZITADEL_ISSUER_URL` di `.env` gak match dengan `iss` claim di JWT | Decode token di jwt.io; pastikan issuer sama |
| Container `tiktok-*` masih ada setelah `make down` | Orphan dari compose project lama `tiktok-backend` | `docker compose -p tiktok-backend down --remove-orphans` |

## Makefile targets (post-PR-#36)

| Target | Efek |
|---|---|
| `make up` | `docker compose up -d --build` di MokiBox compose |
| `make down` | `docker compose down` MokiBox compose (keep volumes), **TIDAK sentuh Zitadel** |
| `make logs` | `docker compose logs -f` MokiBox compose |
| `make up-zitadel` | `docker compose -f $(ZITADEL_COMPOSE_DIR)/docker-compose.yml up -d` |
| `make down-zitadel` | Sama tapi `down` |
| `make logs-zitadel` | Sama tapi `logs -f` |
| `make up-all` | `up-zitadel` lalu `up` (urutan benar) |
| `make down-all` | `down` lalu `down-zitadel` |
| `make help` | List semua target |

Override lokasi Zitadel compose:
```bash
make up-zitadel ZITADEL_COMPOSE_DIR=/srv/zitadel
```

Default `ZITADEL_COMPOSE_DIR=./zitadel-compose` (subdir dari
MokiBox). Production setup biasanya sibling (`../zitadel-compose/`)
atau path absolute; override per-invocation atau di shell env.

## Env-var contract (Zitadel stack ↔ MokiBox stack)

| Var | Di-set di | Di-baca di | Value dev |
|---|---|---|---|
| `ZITADEL_ISSUER_URL` | MokiBox `.env` | `api-gateway` | `http://localhost:8080` (Traefik host publish) |
| `ZITADEL_CLIENT_ID` | MokiBox `.env` | `api-gateway` (future: login UI client) | nilai dari Zitadel console |
| `ZITADEL_API_CLIENT_ID` | MokiBox `.env` | `api-gateway` (audience check) | nilai dari Zitadel console |
| `ZITADEL_TARGET_SIGNING_KEY` | MokiBox `.env` | `api-gateway` (webhook HMAC verify) | signing key dari CreateTarget (one-time) |
| `ZITADEL_VERSION` | MokiBox `.env` (referensi saja) | `zitadel-compose` (image tag) | `v4.16.0` |

**PENTING**: MokiBox `.env` hanya menyimpan **client IDs dan
signing key**, bukan Zitadel instance itu sendiri. Kalau deploy
production:

1. Zitadel stack di-host terpisah (subdomain `auth.yourdomain.com`).
2. `ZITADEL_ISSUER_URL=https://auth.yourdomain.com`.
3. Client ID + signing key di-paste dari Zitadel console / API
   ke MokiBox `.env` (idealnya via secret manager, bukan file).
4. `ZITADEL_EXTERNALSECURE=true` di Zitadel env (bukan MokiBox).

## Production VPS multi-tenant: aaPanel edge (topologi terkunci)

Produksi berjalan di VPS multi-tenant yang port :80/:443-nya sudah
dipakai aaPanel nginx milik host. Konsekuensi: **`mokibox-nginx`
container TIDAK jalan di VPS** — aaPanel nginx adalah edge untuk
semua domain MokiBox.

```
Internet → aaPanel nginx host (:80/:443, TLS wildcard DNS-01)
  ├─ mokiboxapi.<domain> → proxy_pass 127.0.0.1:8180 → api-gateway
  └─ auth.<domain>       → proxy_pass 127.0.0.1:8181 → Zitadel Traefik
```

| Peran | Pemilik |
|---|---|
| TLS termination, HSTS, security headers | aaPanel vhost (translate dari `deploy/nginx/default.conf`) |
| api-gateway | publish loopback `127.0.0.1:8180:8080` |
| Zitadel Traefik | publish loopback `127.0.0.1:8181:80` |
| mokibox-nginx | tidak dipakai (drop via overlay `profiles: [disabled]`) |
| demo UI `/demo/` | static alias aaPanel (file di host, tanpa container) |

Aturan turunan (wajib untuk sesi deploy di VPS):

- **Semua compose command di VPS eksplisit dua `-f`**:
  `docker compose -f docker-compose.yml -f docker-compose.prod.yml ...`
  — dev override TRACKED di repo, jadi compose polos di clone VPS
  akan auto-load config dev (local.conf, port dev, alias domain
  lokal) dan prod jalan broken.
- Publish loopback (`127.0.0.1:<port>:<container-port>`) untuk
  setiap proxy target aaPanel — bukan interface publik; proxy_pass
  ke `127.0.0.1` juga kebal pola stale-DNS upstream.
- Gateway→Zitadel + webhook Zitadel→gateway lewat URL publik https
  — deny-list SSRF default tetap utuh; JANGAN bawa bypass
  deny-list dev ke prod.
- Repo di VPS ditaruh di `/www/wwwroot/<project>` (konvensi
  aaPanel, konsisten dengan site lain di host — bukan home dir),
  checkout branch `deploy/<target>` yang di-pull server, bukan
  `main`.
- `.env` prod: semua secret fresh, di-generate ON-VPS (script
  generator ter-upload yang memakai `openssl rand`), jangan copy
  `.env` dev. Tulis file di VPS via `sftp_upload` lalu eksekusi —
  heredoc multi-line lewat run_command MCP rentan timeout.
- `docker compose up` dengan `.env` kosong/parasial bikin postgres
  crash-loop DAN meninggalkan state init broken di volume →
  `down -v` + re-up dengan `.env` lengkap (hanya kalau volume
  memang fresh, tanpa data).
- Cloudflare proxy (orange cloud) di depan domain: SSL mode WAJIB
  Full (strict) — Flexible = redirect loop; wildcard cert DNS-01
  tetap wajib di aaPanel untuk jalur CF→origin, dan challenge
  http-01 tidak akan lolos lewat proxy CF.
- aaPanel vhost adalah shared surface (domain tenant lain hidup
  di sana): backup file vhost sebelum edit, `nginx -t` sebelum
  reload, hanya TAMBAH file baru — jangan edit vhost existing.
- Backup strategy `zitadel-postgres-1` volume tetap berlaku
  (eventstore irreplaceable; Zitadel recovery = re-create users
  kalau kehilangan eventstore).

## Yang TIDAK berubah (Fase 0 → sekarang)

- `migrations/001_init.sql` (forward-only, tidak disentuh)
- `sqlc/queries/*` (sqlc-generated queries tidak depend on topology)
- `shared/*` (semua package Go di `shared/` polos — gak ada
  referensi docker compose atau Zitadel)
- `api-gateway/*` code (handler + middleware hanya baca env var
  via `shared/config.go`; tidak tahu di mana Zitadel running)

## Lihat juga

- `planning/LLD_PLAN.md` Asumsi A11 (rationale + impact table)
- `planning/PRD.md` NFR-11, FR-AUTH-04, Struktur Direktori
- `references/fase-3-implementation-notes.md` section
  "Minimal main pattern" — `api-gateway` baca Zitadel via
  env, TIDAK via internal docker service name
- `references/fase-3-implementation-notes.md` section
  "Smoke test pattern: docker network + out-of-tree binary"
  — recipe verifikasi E2E signed-webhook (gunakan network
  `tiktok-backend_backend` yang sudah ada, ATAU network
  `mokibox_backend` post-rename)
