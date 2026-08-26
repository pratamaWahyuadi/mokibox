# Threat Report — TikTok Clone Backend MVP

Berikut daftar threat yang paling relevan terhadap domain aplikasi (video sharing + sosial) dan tech stack yang dipilih (Go/Echo, Zitadel self-host, PostgreSQL 16, Redis + Asynq, Cloudflare R2, FFmpeg, Docker Compose single VPS). Diurutkan dari severity tertinggi.

---

## 1. Malicious Video File → RCE / Crash pada Transcoder Worker

- **Threat Actor:** User terautentikasi (termasuk akun Google burner).
- **Vector:** User meminta `upload-intent`, meng-upload file berbahaya — bukan video valid, atau file yang mengeksploitasi celah parser FFmpeg — langsung ke R2 via presigned URL, lalu memanggil `confirm`. Worker mendownload file dan langsung memprosesnya dengan FFmpeg tanpa validasi konten. Jika FFmpeg memiliki CVE pada demuxer/decoder tertentu, attacker bisa mendapatkan code execution di dalam container worker.
- **Impact:** Attacker mengambil alih worker, mencuri environment variable (R2 secret, DATABASE_URL), pivot ke service lain dalam Docker network, menghapus/memanipulasi data, atau menghentikan pipeline transcode. Ini risiko terbesar karena worker memegang akses ke infrastruktur inti.
- **Severity:** Critical
- **Mitigation:**
  - Validasi file sebelum transcode: jalankan `ffprobe` untuk memastikan file benar-benar video, cek container/magic number, batasi codec, resolusi, durasi, dan bitrate.
  - Setelah upload, verifikasi ukuran aktual objek di R2 sesuai klaim user, bukan hanya `file_size` dari body.
  - Jalankan worker sebagai non-root, dengan `read_only` root filesystem, drop capabilities, dan resource limit (CPU, memory, disk).
  - Batasi akses worker ke DB: gunakan role PostgreSQL yang hanya bisa update status video tertentu, bukan superuser.
  - Selalu jalankan FFmpeg versi terbaru dengan patch keamanan.

---

## 2. Redis Queue Exposure / Tanpa Auth → Task Poisoning & DoS

- **Threat Actor:** Attacker yang bisa mengakses port 6379, atau container lain yang berhasil dikompromikan.
- **Vector:** Docker Compose mem-publish Redis ke `0.0.0.0:6379` tanpa password/ACL. Jika VPS tidak punya firewall ketat, attacker bisa `FLUSHALL` (menghapus semua job transcode), atau enqueue task `transcode:video` palsu dengan payload arbitrary. Worker Asynq akan memproses task palsu dan menyedot CPU/disk.
- **Impact:** Video stuck di status `PROCESSING`, pipeline transcode berhenti, resource VPS habis, dan biaya operasional membengkak.
- **Severity:** High
- **Mitigation:**
  - Jangan publish port Redis ke host. Biarkan Redis hanya bisa diakses dari Docker network internal.
  - Aktifkan password/ACL Redis.
  - Di worker, validasi setiap job: cek `video_id` ada di DB, status masih `PROCESSING`, dan `r2_key` cocok dengan record sebelum memproses.
  - Pisahkan Redis untuk queue dari Redis untuk cache/data lain jika memungkinkan.

---

## 3. IDOR pada Endpoint Video, Komentar, dan Sosial

- **Threat Actor:** User terautentikasi biasa.
- **Vector:** Endpoint seperti `DELETE /api/videos/:id`, `DELETE /api/comments/:id`, `GET /api/videos/:id`, dan `POST /api/videos/:id/like` tidak melakukan pengecekan kepemilikan/visibility. Attacker bisa menghapus video atau komentar milik user lain jika UUID-nya diketahui (bisa bocor dari feed, notifikasi, atau riwayat). Video dari akun private juga bisa dibaca langsung via `GET /api/videos/:id` tanpa memeriksa relasi follow.
- **Impact:** Kehilangan data (video/komentar dihapus orang lain), pelanggaran privasi (video private terbaca), dan manipulasi konten.
- **Severity:** High
- **Mitigation:**
  - Terapkan object-level authorization terpusat di tiap handler:
    - Delete video/komentar hanya boleh jika `user_id` pemilik == `currentUser.id`.
    - Baca video detail harus memperhitungkan `is_private` dan relasi follow.
    - Like/comment/unfollow hanya valid jika target benar-benar ada dan visibility mengizinkan.
  - Jangan mengandalkan UUID sebagai mekanisme keamanan.

---

## 4. Webhook Zitadel Tanpa Verifikasi → Spoofed User Deletion/Deactivation

- **Threat Actor:** Attacker yang bisa memanggil endpoint webhook, atau memicu SSRF.
- **Vector:** Backend menerima event dari Zitadel (misal `user.deleted`) untuk menjaga konsistensi data user. Jika endpoint webhook tidak memverifikasi signature/secret Zitadel, attacker bisa mengirimkan event palsu yang mengaku user tertentu dihapus/dinonaktifkan. Backend kemudian menghapus user beserta video, komentar, like, follow, dan notifikasi.
- **Impact:** Akun user hilang secara permanen, konten hilang, dan jika dikirim massal bisa menghancurkan seluruh data platform.
- **Severity:** High
- **Mitigation:**
  - Verifikasi signature/secret webhook Zitadel dengan constant-time comparison.
  - Hanya terima request dari IP/sumber yang dikenal jika memungkinkan.
  - Gunakan HTTPS untuk endpoint webhook.
  - Rate limit endpoint webhook dan log semua event untuk audit.

---

## 5. R2 Bucket Misconfigured / URL Statis → Video Private Bocor

- **Threat Actor:** Siapa saja yang mendapatkan URL objek.
- **Vector:** Skema menyimpan `hls_url` dan `thumbnail_url` sebagai path statis seperti `hls/{id}/master.m3u8`. Jika bucket R2 di-set public-read, atau API mengembalikan path tersebut dan klien bisa mengaksesnya langsung tanpa autentikasi, maka semua video — termasuk dari akun private — bisa diputar oleh siapa saja yang memiliki URL. Thumbnail juga bisa bocor.
- **Impact:** Information disclosure seluruh konten video, pelanggaran privasi, dan potensi biaya bandwidth dari akses tidak sah.
- **Severity:** High
- **Mitigation:**
  - Pastikan bucket R2 dalam mode private.
  - API tidak pernah mengembalikan URL statis; melainkan presigned URL berumur pendek (misal 15–60 menit) untuk file HLS dan thumbnail.
  - Jika ingin performa, gunakan Cloudflare Token Authentication / signed cookies untuk akses media, bukan bucket publik.
  - Routikan semua akses media melalui API atau CDN yang memiliki kontrol akses.

---

## 6. JWT/Zitadel Misconfiguration → Token Forgery atau Token Leak

- **Threat Actor:** Attacker eksternal, atau user yang ingin memalsukan identitas.
- **Vector:** Kode contoh di ide menggunakan fungsi `validateJWT` custom tanpa detail verifikasi. Jika implementasi hanya memparsing claims dan tidak memverifikasi signature, `issuer`, `audience`, atau algoritma, attacker bisa membuat token palsu. Selain itu, jika Nginx tidak memaksa HTTPS, access token JWT bisa dicuri via network sniffing saat dikirim dari mobile app.
- **Impact:** Account takeover penuh: attacker bisa melakukan aksi sebagai user mana pun, termasuk upload video berbahaya, hapus konten, dan baca data private.
- **Severity:** High
- **Mitigation:**
  - Gunakan library OIDC resmi (`coreos/go-oidc`) untuk validasi token: verifikasi signature via JWKS Zitadel, cek `issuer`, `audience`, `expiry`, dan `alg` yang diizinkan.
  - Jangan pernah menerima token dengan `alg: none` atau `HS256` untuk token yang seharusnya ditandatangani RSA.
  - Pastikan seluruh traffic API melalui HTTPS di Nginx, aktifkan HSTS.
  - Konfigurasi Zitadel issuer URL dengan domain publik yang benar dan jangan mengubahnya setelah production berjalan.

---

## 7. Upload Tanpa Batas Ukuran/Konten → Storage & Biaya Membengkak, Worker DoS

- **Threat Actor:** User terautentikasi (akun Google mudah dibuat).
- **Vector:** `upload-intent` hanya menerima `file_size` dari body request yang bisa dimanipulasi. Presigned URL tidak dibatasi ukuran objek. User bisa meng-upload file raksasa atau file non-video ke bucket R2, lalu confirm. Worker akan mencoba men-download dan memprosesnya, menghabiskan disk `/tmp`, CPU, dan kuota R2.
- **Impact:** Biaya infrastruktur melebihi budget, VPS kehabisan disk (kegagalan yang paling ditakutkan), dan pipeline transcode berhenti.
- **Severity:** High
- **Mitigation:**
  - Terapkan condition `content-length-range` pada presigned URL R2, misal min 1 KB dan max 200 MB.
  - Pastikan `file_size` di `upload-intent` divalidasi server-side dan cocok dengan ukuran aktual saat `confirm`.
  - Batasi durasi maksimal video (misal 3 menit) dan resolusi/bitrate di worker.
  - Set timeout per job transcode (misal 5 menit untuk video 60 detik), lalu kill proses FFmpeg yang melebihi batas.

---

## 8. Tidak Ada Rate Limiting → Spam Sosial & Notification Bombing

- **Threat Actor:** Bot / user terautentikasi.
- **Vector:** Endpoint `like`, `comment`, `follow`, `view` tidak memiliki rate limit. Satu user bisa memanggil `POST /api/videos/:id/view` ribuan kali untuk menggelembungkan `views_count`, melakukan spam comment, atau follow/unfollow berulang. Ini juga membanjiri tabel notifikasi dan memberatkan PostgreSQL.
- **Impact:** Kualitas data feed rusak, notifikasi tidak berguna, resource API/DB terpakai berlebihan, dan pengalaman user terganggu.
- **Severity:** Medium
- **Mitigation:**
  - Tambahkan rate limiter per user (atau per IP) di API Gateway untuk semua endpoint mutasi, misal 30 request/menit untuk like/follow/comment dan 60 request/menit untuk view.
  - Batasi panjang komentar dan frekuensi pembuatan notifikasi.
  - Pertimbangkan batasan harian untuk operasi sosial per user.

---

## 9. Default Credentials & Port Internal Terbuka → Akses Langsung ke DB/Redis

- **Threat Actor:** Attacker yang mendapatkan akses jaringan ke VPS, atau melalui service lain yang berhasil dieksploitasi.
- **Vector:** Docker Compose menggunakan password Postgres default `tiktok`, port `5432` dan `6379` di-publish ke host. Jika port tidak dibatasi firewall dan attacker berhasil masuk ke jaringan VPS (misal via service yang lebih lemah), dia bisa connect langsung ke PostgreSQL dan Redis. Kredensial juga berisiko bocor jika `.env` ikut ter-commit ke repository.
- **Impact:** Data breach seluruh data user dan video, manipulasi DB, dan akses ke R2 secret.
- **Severity:** Medium
- **Mitigation:**
  - Ganti semua default password dengan credential kuat yang disimpan di environment variable VPS, bukan di repo.
  - Hanya expose port yang benar-benar dibutuhkan ke publik: Nginx (80/443) dan API Gateway. Jangan publish port Postgres/Redis ke host.
  - Batasi akses SSH, aktifkan firewall (ufw/iptables), dan pisahkan jaringan Docker untuk service internal.
  - Gunakan Docker secrets atau file `.env` dengan permission ketat (`chmod 600`).

---

## 10. Brute-Force / Credential Stuffing pada Zitadel Self-Host

- **Threat Actor:** Attacker eksternal.
- **Vector:** Zitadel di-deploy self-host dan hosted UI-nya terekspos ke internet via subdomain. Jika tidak ada rate limit, lockout, atau password policy yang kuat, attacker bisa melakukan brute-force password akun user atau credential stuffing memakai kombinasi email/password dari kebocoran data lain. Google Sign-In aktif, tapi user dengan password lemah tetap berisiko.
- **Impact:** Account takeover; attacker bisa upload video berbahaya, hapus data user, atau menyalahgunakan akun untuk spam.
- **Severity:** Medium
- **Mitigation:**
  - Gunakan fitur security Zitadel: rate limit login, lockout setelah gagal berulang, dan password policy kuat.
  - Aktifkan MFA untuk akun stakeholder/internal.
  - Awasi log login Zitadel untuk pola mencurigakan.
  - Pastikan subdomain Zitadel hanya bisa diakses via HTTPS.

---

# Prioritas Wajib untuk MVP

Item dengan Severity **Critical/High** berikut wajib menjadi acuan di PRD dan harus dimitigasi sebelum rilis:

1. **Malicious Video File → RCE pada Transcoder Worker** — Critical. Wajib ada validasi file + isolasi worker.
2. **Redis Queue Exposure / Task Poisoning** — High. Wajib amankan jaringan internal Redis + validasi job.
3. **IDOR pada Endpoint Video/Komentar/Sosial** — High. Wajib ada object-level authorization.
4. **Webhook Zitadel Tanpa Verifikasi** — High. Wajib verifikasi signature event Zitadel.
5. **R2 Bucket Misconfigured / URL Statis** — High. Wajib bucket private + presigned URL untuk semua akses media.
6. **JWT/Zitadel Misconfiguration** — High. Wajib validasi OIDC penuh + HTTPS.
7. **Upload Tanpa Batas Ukuran/Konten** — High. Wajib batasi ukuran upload + validasi konten sebelum transcode.

---

# Pertimbangkan untuk Iterasi Berikutnya

Item dengan Severity **Medium/Low** tidak wajib di MVP, tetapi sebaiknya dicatat sebagai rencana perbaikan:

- **Rate limiting untuk endpoint sosial** (Medium).
- **Perkuat credential service internal & firewall port** (Medium).
- **Brute-force protection pada Zitadel self-host** (Medium).
- Tambahan untuk iterasi berikutnya: structured logging, monitoring queue/worker, dan audit trail untuk insiden keamanan.