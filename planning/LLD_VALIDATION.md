## Kontradiksi Tech Stack

1. **Presigned URL upload tidak memakai `content-length-range` sesuai PRD**  
   - PRD (FR-VIDEO-01) menyebut:  
     > `POST /api/videos/upload-intent` menghasilkan presigned URL R2 dengan kondisi **`content-length-range`**: minimum 1 KB, maksimum 200 MB.  
   - PRD (SEC-08) juga menyebut:  
     > Presigned URL upload dibatasi `content-length-range`, cek ukuran aktual di confirm.  
   - Coding Plan (Asumsi A3) justru menyatakan:  
     > S3/R2 presigned PUT tidak mendukung kondisi `content-length-range`. ... Ukuran file dipaksa divalidasi di `confirm` via `HeadObject` R2.  
   - Ini kontradiksi langsung dengan requirement PRD. LLD mengganti mekanisme yang diminta PRD dengan compensating control, padahal PRD menetapkan batasan tersebut sebagai requirement keamanan wajib.

2. **Pilihan library validasi JWT menyimpang dari opsi yang tertulis di PRD**  
   - PRD Tech Stack:  
     > Validasi JWT: `coreos/go-oidc` atau `golang-jwt/jwt + JWKS manual`.  
   - Coding Plan Fase 3:  
     > Pakai SDK resmi `github.com/zitadel/zitadel-go/v3` ... **bukan** `coreos/go-oidc`.  
   - Ini bukan kontradiksi fatal karena PRD memberi ruang “atau sejenis”, tetapi LLD memilih library yang tidak tercantum eksplisit di PRD. Perlu dicatat sebagai deviasi pilihan teknis.

---

## Asumsi Tambahan di LLD yang Tidak Ada di PRD

1. **Library auth `zitadel-go/v3` dipakai untuk validasi access token JWT**  
   PRD hanya menyebut `coreos/go-oidc` atau `golang-jwt/jwt`. Coding Plan memilih `zitadel-go/v3` dengan `oauth.DefaultJWTAuthorization`, yang tidak disebut di PRD.

2. **Zitadel Actions V2 Target menggantikan istilah “webhook”**  
   PRD berkali-kali menyebut “webhook Zitadel”. LLD mengasumsikan Zitadel tidak punya webhook generik, sehingga memakai **Actions V2 Target** dengan header `ZITADEL-Signature` dan helper `actions.ValidateRequestPayload`. Ini keputusan integrasi yang tidak dijelaskan PRD.

3. **Media token HMAC untuk akses playlist HLS**  
   PRD hanya mensyaratkan “signed playlist” / presigned URL. LLD menambahkan mekanisme spesifik: HMAC token 15 menit yang terikat ke `video_id`, bisa dipakai lewat query parameter `?token=`. Ini detail teknis baru.

4. **Endpoint tambahan `DELETE /api/users/me` dan `GET /api/videos/:id/playlist.m3u8`**  
   PRD tidak mencantumkan kedua endpoint ini di tabel API. LLD menambahkannya untuk memenuhi US-09 / FR-USER-03 dan FR-VIDEO-10.

5. **Tombstone `users` dengan `deleted_at` dan username placeholder**  
   PRD hanya menyebut `is_active = false` dan penghapusan data lokal. LLD menambahkan `deleted_at`, null PII, dan `username = 'deleted_<uuid>'` agar JWT lama tidak membuat user baru lagi. Ini keputusan schema yang tidak eksplisit di PRD.

6. **Detail schema tambahan**  
   LLD menambahkan `actor_id` di `notifications`, partial unique index satu `PENDING_UPLOAD` per user, composite foreign key `(parent_id, video_id)` di `comments`, serta berbagai check constraint. Semua konsisten dengan PRD tapi tidak disebut di PRD.

7. **Pembagian role PostgreSQL `tiktok_api` dan `tiktok_worker`**  
   PRD hanya bilang worker pakai role terbatas. LLD mengkonkretkan role `tiktok_worker` hanya memiliki `SELECT, UPDATE, DELETE` pada `videos`, dan `tiktok_api` memiliki akses CRUD semua tabel.

8. **Detail isolasi worker**  
   PRD sudah mewajibkan non-root, read-only filesystem, drop capabilities, dan resource limit. LLD menambahkan detail seperti `tmpfs /tmp`, `pids_limit: 200`, `security_opt: no-new-privileges`, `user: "10001:10001"`. Ini not necessarily kontradiksi, tapi asumsi teknis tambahan.

---

## Kesimpulan

Secara umum tech stack utama sudah konsisten dengan PRD: **Go 1.22+, Echo, PostgreSQL 16, sqlc + pgxpool, Redis + Asynq, Cloudflare R2, aws-sdk-go-v2, FFmpeg/ffprobe, Nginx, Docker Compose di satu VPS**. Database schema dan pendekatan akses data juga sesuai.

Namun **tidak bisa dikatakan konsisten penuh** karena ada satu kontradiksi requirement: PRD mewajibkan presigned URL upload dengan `content-length-range`, sedangkan Coding Plan menggantinya dengan validasi ukuran hanya di `confirm`. Ada juga beberapa asumsi teknis besar di LLD yang tidak eksplisit di PRD, terutama pemilihan `zitadel-go/v3` dan mekanisme Actions V2 Target. Keputusan tersebut sebaiknya dikonfirmasi ulang ke user sebelum implementasi.