# Fase 10 Verification Notes — issue #29 (unit tests) + #39 (FFmpeg timeout-kill), PR #47

Session 2026-09-03, branch `feature/phase-10-tests`. Dokumen ini
menyimpan detail yang tidak muat di SKILL.md: repro timeline,
nomor timing, commands yang benar, dan dead-ends yang sudah
diukur (bukan spekulasi).

## 1. Bug ctx-expired `MarkVideoFailed` (pre-existing sejak fase 5)

### Repro (live, smoke run 5 — worker TRANSCODE_TIMEOUT=2s, input 174MB)

```
11:36:13 start  (attempt 1)
11:36:15 ERROR "download raw" context deadline exceeded   <- kill fires
           INFO  re-enqueued retry_count=1 delay=30s
...      (attempt 2, sama, retry_count=2 delay=60s)
...      (attempt 3, sama)
           ERROR "mark failed (post-budget)" context deadline exceeded  <- BUG
           INFO  "budget exhausted after transient failure" retry_count=3
           INFO  HandleCleanupObjects: start / success
```

Row final: `status=PROCESSING, retry_count=3` — BUKAN FAILED.
`MarkVideoFailed` dijalankan dengan task-ctx yang sama yang barusan
membunuh pipeline → `context deadline exceeded` → UPDATE tidak pernah
mendarat. Row stuck selamanya: status-poll user hang di PROCESSING,
24h cleanup job tidak menyentuhnya (butuh status=DELETED), retry
budget habis permanen.

### Fix

```go
func (w *Worker) markFailedDetached(videoID uuid.UUID) error {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    _, err := w.Queries.MarkVideoFailed(ctx, videoID)
    return err
}
```

4 call site dimigrasi: post-budget (HandleTranscode), invalid-media
(ProbeFile branch), validate (ValidateMedia branch), post-budget
(handleTransient). `MarkVideoReady` SENGAJA tidak — dijaga
`WHERE status='PROCESSING'` + re-check + 24h cleanup; migrasi
melebarkan window unbounded work.

Bukti post-fix: smoke run 6+ → `row FAILED with retry_count=3`
(PASS), konsisten di semua run berikutnya.

## 2. Fixture sizing untuk kill-verification (data live)

CPU VPS encode 480p veryfast sangat cepat; throughput VPS→R2
(cloudflarestorage.com) **~2.3 MB/s** diukur dari timing download
9.2MB = 4.0s penuh di worker log.

| Eksperimen | Size | Re-encode worker-like | Hasil |
|---|---|---|---|
| mandelbrot 960x540 maxiter90k 20s | 18.9 MB | 0.5s | REJECTED — lihat di bawah |
| mandelbrot 1280x720 maxiter120k 60s | 174 MB | 4.4s | download selalu kena kill (run 2-3) |
| noise 720p crf35 15s | 60 MB | 1.75s | downscale terlalu murah |
| noise 480p crf35 30s | 49 MB | 2.32s | borderline, size masih masalah |
| noise 480p crf35 40s | 65 MB | 2.91s | re-encode OK tapi download 65MB >4s |
| noise 480p crf40 40s | 3.1 MB | 1.22s | terlalu murah |
| **noise 480p crf40 120s** | **9.2 MB** | **3.62s** | **dipakai** — download ~2-4s, encode tetap kena |

**Lesson mandelbrot** (Technical Notes issue #39 menyarankan mandelbrot,
tapi salah untuk use case ini): mandelbrot mahal per-frame saat
DIGENERATE (maxiter), tapi begitu di-encode lossy (H.264 CRF),
intra-frame complexity-nya collapse — re-encode jadi 0.5s.
"Mahal saat generate ≠ mahal saat re-encode." Random noise yang
bertahan through lossy round-trip.

**Lesson konklusif**: kill mendarat di step yang sedang memegang
budget saat deadline fires. Di VPS ini selalu download. Membuat
"download cepat + encode mahal" terbukti fisik tidak tercapai
(cheap-reencode selalu small; expensive selalu large). Solusi:
2-layer (runtime smoke untuk ladder + unit test runFFmpeg untuk
ffmpeg-kill spesifik) — user-approved.

## 3. Pattern worker-swap untuk timeout verification

```bash
# WAJIB urutan ini — rebuild dulu supaya replacement jalan dari
# binary CURRENT (stale image = silently test binary lama):
docker compose build transcoder-worker
docker compose stop transcoder-worker
docker run -d --name mokibox-transcoder-worker-timeout \
  --network mokibox_backend \
  -e WORKER_DATABASE_URL=... -e REDIS_ADDR=... -e REDIS_PASSWORD=... \
  -e R2_*... -e TRANSCODE_TIMEOUT=4s \
  mokibox-transcoder-worker:dev
# ... assertions ...
# restore SEBELUM positive control (user decision) supaya control
# jalan di budget production 5m, bukan budget pendek:
docker rm -f mokibox-transcoder-worker-timeout
docker compose up -d transcoder-worker
# verify: docker exec mokibox-transcoder-worker printenv TRANSCODE_TIMEOUT
```

Catatan: `docker compose stop/up` harus lewat background=true di
Hermes terminal (foreground ditolak sebagai long-lived server).

## 4. Assertion honesty gap (self-caught)

Assertion kill-step awal hidup di polling loop:

```bash
# BUG: hanya-PASS, tidak pernah FAIL kalau tidak match
[[ $saw_kill -eq 0 ]] && grep -q ... && { saw_kill=1; pass "..."; }
# kalau grep tidak match: tidak PASS, tapi juga TIDAK FAIL — silent gap
```

Smoke melaporkan "21 PASS" padahal kriteria inti (kill di ffmpeg)
tidak pernah terpenuhi. Fix: pindahkan kriteria ke assertion
post-loop yang PUNYA cabang FAIL eksplisit, atau laporkan step kill
apa adanya (grep nama step) — jangan assert syarat yang tidak bisa
dipenuhi lalu menutupinya dengan PASS generik.

## 5. Nginx stale-DNS 502 post-reboot

Symptom: integration smoke tiba-tiba 3 PASS/13 FAIL, semua endpoint
502 dari host; `wget http://api-gateway:8080/healthz` dari DALAM
container nginx = 200. Root cause: nginx me-resolve hostname
upstream sekali saat config load; gateway restart (post-VPS-reboot)
1 detik setelah nginx start dan dapat IP baru → nginx stuck di IP lama.
Fix cepat: `docker compose restart nginx` → 16 PASS kembali.

## 6. Minor: test-closure pattern

Test helper yang men-capture nilai handler:
```go
// BUG: seen dikembalikan BY VALUE saat deklarasi (nil) — handler
// mengisi copy yang tidak pernah terlihat test
func newAuthEcho(cfg) (*echo.Echo, *db.User) { var seen *db.User; ...; return e, seen }
// FIX: holder struct — pointer ke struct shared dengan handler
func newAuthEcho(cfg) (*echo.Echo, *userHolder) { h := &userHolder{}; ...; h.user = u ...; return e, h }
```

## Referensi run

- Smoke: `scripts/smoketest/phase10_timeout/run.sh` (22 PASS, rerun 2x exit 0)
- Unit: `transcoder-worker/runffmpeg_test.go` (kill @1.01s + positive control)
- Commits: `9af74d3` (#29), `9385f16` (fix ctx), `8a4f62f` (#39) — PR #47
