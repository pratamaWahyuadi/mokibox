# Roadmap: TikTok Parity (Backend)

Status: **Draft — menunggu approval user**
Tanggal: 2026-09-29
Scope: **backend `api-gateway` + `transcoder-worker` saja**. Aplikasi web & mobile
sudah ada dan sudah memakai endpoint yang ada — issue di dokumen ini TIDAK
termasuk pekerjaan frontend.

---

## 1. Posisi saat ini (verified 2026-09-29, branch `main`)

Fase 0–10 selesai dan merged. Yang **sudah ada**:

| Area | Status |
|---|---|
| Auth | Zitadel OIDC + Google Sign-In, middleware JWT, webhook Actions V2 (deactivate user) |
| Upload | presigned PUT R2 → `upload-intent` → `confirm` → asynq queue |
| Transcode | worker ffmpeg/ffprobe → HLS 480p + 720p, master playlist rewrite, thumbnail, retry budget 3x, cleanup raw, kill-path timeout |
| Feed | `GET /api/feed/home` — **kronologis** (`created_at DESC`), cursor pagination |
| Social | follow/unfollow, followers/following list, like/unlike, comment + reply, view tracking (1 view per request) |
| Notifikasi | `GET /api/notifications`, `PUT /read-all`; type `follow`/`like`/`comment` |
| Akun | `GET/PUT/DELETE /api/users/me`, tombstone, R2 cleanup via queue |
| Ops | rate limit per-user token bucket (in-process), `/healthz`, unit + integration smoke, SECURITY.md |

**Yang tidak ada sama sekali** (0 baris di `migrations/001_init.sql` dan
`planning/04_api_contracts.md`): `search`, `hashtag`, `sound`, `video_history`,
`favorites`, `blocks`, `mutes`, `reports`, `share_count`, `is_verified`,
`batch view`/`watch_time`, `discover`, admin/moderation API.

---

## 2. Kenapa prioritas ini (argumen produk)

TikTok terasa "TikTok" karena tiga hal yang tidak ada di MokiBox sekarang:

1. **Discovery** — orang bisa menemukan konten tanpa sudah follow siapa pun.
   Sekarang satu-satunya jalan ke konten adalah feed kronologis; video lama
   praktis tidak pernah dilihat oleh orang baru.
2. **Personalisasi** — feed `For You` di-rank, `Following` dipisah. Sekarang
   keduanya tidak ada; semua orang dapat feed yang sama dan `views_count`
   tidak dipakai jadi sinyal.
3. **Retensi & safety** — history, favorit, block, report. Tanpa ini user
   kehilangan kontennya dan tidak bisa memoderate siapa pun.

P1/P2/P3 di bawah P0 adalah lapisan kedalaman (monetisasi tidak akan pernah
dimakai kalau discovery dan retention belum ada).

---

## 3. P0 — 10 issue, dikerjakan sebelum fitur lain

Dipilih dengan urutan ketergantungan. **Issue #6 harus selesai sebelum #7.**

| # | Judul | Label | Cx | Ketergantungan |
|---|---|---|---|---|
| 1 | `GET /api/search` — cari user & video | `P0`, `backend`, `database` | M | migration baru |
| 2 | Migration `003_hashtags.sql` | `P0`, `database` | M | — |
| 3 | Hashtag parse + halaman hashtag | `P0`, `backend`, `database` | M | #2 |
| 4 | Sound/music library | `P0`, `backend`, `database`, `storage` | M | migration baru |
| 5 | `GET /api/feed/following` | `P0`, `backend` | S | — |
| 6 | View batching + watch time + completion rate | `P0`, `backend`, `database` | M | — |
| 7 | For-You ranking v1 | `P0`, `backend`, `queue` | L | **#6** |
| 8 | Watch history | `P0`, `backend`, `database` | M | migration baru |
| 9 | Favorit / saved videos | `P0`, `backend`, `database` | S | migration baru |
| 10 | Block & mute | `P0`, `backend`, `database` | M | migration baru, filter lintas endpoint |

### Kenapa urutan ini

- **#1 (search) independen dari #2/#3 (hashtag)**, tapi search video dan
  query hashtag memakai infrastruktur yang sama (filter TEXT + cursor paging),
  jadi dikerjakan lebih dulu membuat #3 tidak mengulang pattern.
- **#2 wajib sebelum #3** — #3 butuh tabel `video_hashtags` untuk query.
- **#4 (sound) independen** dari #2/#3, tapi soal R2 key layout
  (`sounds/<id>/<variant>.mp3`) harus konsisten dengan upload video yang sudah
  ada. Kalau dikerjakan duluan, concertkan prefix R2 sekaligus.
- **#6 sebelum #7** — tanpa ini ranking hanya bisa pakai `views_count` yang
  sudah tidak bermakna (1 increment per request, bukan per impression nyata).
  Catatan: `IncrementViews` dipanggil setiap `POST /api/videos/:id/view`
  tanpa rate limit per user per video, jadi `views_count` bisa di-inflate
  dengan satu script. **Ini deficiency yang sudah ada sekarang** — #6 harus
  menutupnya sekalian, bukan cuma menambah kolom watch time.
- **#10 (block) dampaknya paling luas** — harus menyentuh feed, search,
  comment list, profile, follow list. Lakukan sebelum query apa pun jadi
  "settled", atau filter block akan ditambahkan 5 kali di 5 tempat berbeda.

---

## 4. P1 — 27 issue (belum dibuat, menunggu P0 selesai)

Keamanan & safety: report content, admin moderation API, auto-filter NSFW.
Retention: share + share link, notification type baru (`reply`/`mention`/
`follow_back`), unread-count endpoint, push FCM.
Konten: edit video, draft, batch upload, resume upload multipart, scheduled post,
subtitle, text overlay.
Social: quote video, comment reaction, suggested accounts, verified badge.
Discovery lanjutan: discover page, hashtag autocomplete.
Analitik: creator analytics endpoint.
Ops: Redis rate limit, CDN + signed cookie, backup Postgres, CI, structured
logging + `/readyz`, idempotency key, test gap.

## 5. P2 — 10 issue (nice-to-have)

Explore-by-interest, kategori video, download original, duet & stitch, live
streaming, AI moderation, watermark, email digest, data export, admin analytics.

## 6. P3 — 5 issue (eval dulu, jangan dijadwalkan)

DM, live go-to + gift, multi-region, monetisasi, A/B testing framework.

---

## 7. Keputusan arsitektur yang sudah diambil (bukan open question)

1. **Migration numbering** — `002_reconcile_users_grant.sql` sudah dipakai.
   P0 butuh 4 migration baru, satu per domain:
   `003_hashtags.sql`, `004_sounds.sql`, `005_video_history.sql`,
   `006_favorites.sql`, `007_blocks.sql`. Satu file per domain supaya
   rollback per fitur possible dan diff-nya reviewable.
2. **`sqlc.yaml` perlu di-update** — sekarang `schema:` hanya list
   `../migrations/001_init.sql`. Issue migration baru **wajib** menambahkan
   file-nya ke list itu, kalau tidak sqlc tidak pernah tahu kolom baru dan
   generate ulang jadi stale. Ini belum ter-tag di issue mana pun → sudah
   jadi acceptance criteria eksplisit di issue #2, #4, #8, #9, #10.
3. **Ranking For-You** — pakai PostgreSQL query dengan scoring SQL
   (`GREATEST`/`LEAST` clamp, `EXTRACT(EPOCH FROM now() - created_at)` untuk
   freshness), bukan service ranking terpisah atau Redis sorted set. Alasannya:
   data belum cukup besar, satu query sudah cukup, dan tidak menambah komponen
   yang harus di-deploy. Kalau later butuh ML ranking, `ListFeedVideos` jadi
   titik substitusi yang jelas.
4. **Block/mute** — `blocks` (sembunyi total, dua arah) dan `mutes`
   (satu arah, hanya feed) **dipisah**, bukan satu tabel dengan flag. Ini
   semantik berbeda dan konsekuensinya akan muncul di production.

## 8. Yang TIDAK boleh lupa saat mengerjakan issue P0

- Error handling: `shared.Wrap(sentinel, ctx)` + `shared.RespondError`. Tidak
  boleh `c.JSON` manual. Mapping status hanya di `shared.ClassifyError`.
- Missing row = `sql.ErrNoRows` saja. `grep -rn "pgx.ErrNoRows"` harus 0 match.
- Anti-enumeration: unauthorized read = **404**, bukan 403. Pakai helper yang
  sudah ada di `api-gateway/handlers/visibility.go`
  (`assertVideoOwnerOrVisible` / `assertVideoReadyVisible`) — jangan tulis ulang.
- Notification insert: like/comment **in-tx**, follow **best-effort out-of-tx**.
- Counter tidak boleh di-zero oleh tombstone; yang meng-zero adalah worker.
- No raw SQL di handler — semua lewat `sqlc/queries/*.sql` + `make sqlc-gen`.
- Handler baru wajib punya unit test di `api-gateway/handlers/*_test.go` dengan
  mock store (pola ada di `social_test.go`).
- Commit: `feat: [phase-11.N] <deskripsi>` — konvensi repo pakai **titik +
  1 digit**, bukan `phase-N-01`.
