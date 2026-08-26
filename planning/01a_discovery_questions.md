Terima kasih, ide-nya sudah sangat detail. Sebelum PRD dan LLD ditulis, saya perlu menggali beberapa hal yang belum eksplisit dan yang akan langsung memengaruhi keputusan teknis. Tolong jawab sejujur-jujurnya — jawaban yang kenapa-kenapa justru paling berguna.

## 1. Skala & Beban Sistem
1.  Berapa target total registered users untuk MVP? (100? 1.000? 10.000?)
2.  Berapa estimasi concurrent active user saat launch? Apakah ada pola jam tertentu yang lebih ramai?
3.  Berapa target video di-upload per hari dan per bulan? Total durasi video estimasi berapa menit?
4.  Berapa estimasi volume data baru per hari dalam GB? (ini akan menentukan kapasitas R2 dan bandwidth)
5.  Apakah ada skenario lonjakan traffic yang sudah diprediksi? Misal demo hari pertama, rilis publik, atau event tertentu?

## 2. Timeline & Prioritas
6.  Apakah deadline 2 minggu itu hard deadline mutlak, atau bisa mundur beberapa hari kalau dibutuhkan?
7.  Kalau waktu mepet, fitur apa yang paling bisa dikorbankan: notifications, reply comment, private account, fitur delete video, atau yang lain?
8.  Apakah aktivitas "testing + polish" di hari 13-14 boleh dikorbankan demi fitur tambahan, atau justru non-negotiable?
9.  Apakah MVP ini untuk production public, demo internal ke stakeholder, atau proof-of-concept?

## 3. Budget & Constraint Infrastruktur
10. Berapa budget bulanan untuk infrastruktur? (compute, storage, bandwidth, biaya Zitadel, dll)
11. Zitadel mau di-self-host dalam docker-compose yang sama, atau pakai Zitadel Cloud? Kalau self-host, apakah tim siap menanggung operational burden-nya?
12. Apakah semua komponen wajib jalan di satu VPS / satu docker-compose, atau boleh tersebar di beberapa service?
13. Ada tidak batasan compliance atau data residency? Misal data harus di region tertentu, atau tidak boleh keluar dari negara tertentu.

## 4. Tech Stack — Bahasa & Framework
14. Kenapa memilih Go + Echo? Seberapa familiar tim dengan kombinasi ini?
15. Versi Go yang akan dipakai? (misal 1.22+, 1.23+)
16. Apakah boleh menambah library pihak ketiga selain Echo? Misal validator, JWT helper, UUID library — atau ada keinginan meminimalkan dependency sebanyak mungkin?

## 5. Tech Stack — Database
17. PostgreSQL 16 itu requirement yang sudah final, atau bisa 15/17 asalkan fitur yang dipakai sama?
18. Apakah ada extension database yang dibutuhkan selain yang default? Misal `pgcrypto`, `pg_trgm`, atau yang lain?
19. Apakah single instance PostgreSQL cukup untuk MVP, atau sudah ada rencana read replica / cluster?
20. Untuk migrasi schema: tetap pakai file SQL yang dijalankan manual via psql, atau butuh tool migration seperti golang-migrate / tern / atlas?

## 6. Tech Stack — Akses Data
21. Contoh kode di ide kamu campur antara `h.db.Create` (gaya ORM/GORM) dan `db.Exec` / `db.Raw` (gaya raw SQL). Keputusan final: standar ke GORM penuh, raw SQL penuh (`pgx` + `database/sql`), query builder, atau memang sengaja campur?
22. Kalau harus memilih prioritas untuk akses data, mana yang lebih penting: kecepatan development, kontrol penuh atas query, type safety, atau kemudahan dipelajari tim?
23. Apakah butuh koneksi pooling yang dikonfigurasi eksplisit, atau default library sudah cukup?
24. Apakah ada kebutuhan rollback strategy untuk schema migration kalau deploy gagal?

## 7. Tech Stack — Frontend
25. Apakah ini murni API backend? Game mana yang akan konsumsi: mobile app yang sudah ada, tim lain yang sedang bangun, atau belum ada?
26. Kalau mobile app ada, platform-nya apa: iOS native, Android native, Flutter, atau React Native?
27. Apakah perlu web frontend sederhana untuk demo/testing, atau cukup Postman/curl?
28. Apakah ada kebutuhan halaman admin untuk moderasi (hapus video/komentar user), atau memang tidak perlu di MVP?

## 8. Tech Stack — Auth
29. Flow login-nya seperti apa: user dibawa ke hosted UI Zitadel, atau API perlu dukung login username/password langsung ke Zitadel?
30. Apakah access token JWT saja cukup, atau butuh refresh token juga? Bagaimana behavior saat token expired?
31. Role/permission apa saja yang harus didukung? Semua user sama, atau perlu admin/moderator?
32. Kalau user dihapus/dinonaktifkan di Zitadel, apakah sistem harus mendengar webhook dan ikut menonaktifkan data user di PostgreSQL?
33. Apakah satu user bisa login dari banyak perangkat sekaligus? Kalau iya, perlu fitur "logout semua perangkat" atau tidak?

## 9. Tech Stack — Deployment
34. Target deploy final: VPS pribadi, PaaS seperti Railway/Fly, atau platform lain?
35. Apakah docker-compose yang ditulis ini akan dipakai langsung di production, atau hanya untuk development?
36. Butuh CI/CD otomatis (misal GitHub Actions), atau cukup `git pull` + `docker-compose up -d` manual?
37. Sudah punya domain dan reverse proxy (Caddy/Nginx/Traefik)? Ini relevan untuk Zitadel issuer URL dan presigned URL R2.

## 10. Integrasi Eksternal
38. Integrasi wajib selain R2, Zitadel, dan FFmpeg — misal push notification FCM/APNs, email service, atau layanan analitik? Atau eksplisit tidak perlu?
39. Apakah perlu integrasi dengan layanan moderasi konten (misal deteksi NSFW/hate speech) atau memang di luar scope MVP?
40. Untuk login, apakah Zitadel perlu terhubung ke Google/Apple sign-in, atau cukup email/password internal saja?

## 11. Data & Privasi
41. Data yang tersimpan di PostgreSQL dan R2 — apakah ada PII yang perlu perlakuan khusus? Misal email asli, nomor HP, atau identitas lain dari Zitadel yang ikut tersimpan?
42. Apakah perlu fitur "delete account" yang benar-benar menghapus semua data user: profil, video, komentar, like, follow, notifikasi, dan file di R2?
43. Adakah requirement retensi data? Misal video yang dihapus harus hilang dari R2 dalam X hari, atau log tidak boleh disimpan lebih dari Y hari.
44. Apakah perlu audit log untuk aktivitas moderasi/admin?

## 12. Offline / Sinkronisasi
45. Apakah mobile app perlu mode offline? Misal user bisa menyimpan draft video lokal, atau like/comment dilakukan offline lalu di-sync nanti?
46. Kalau iya, apakah API perlu mendukung idempotency key untuk operasi seperti like/comment supaya tidak dobel saat client retry?
47. Atau apakah MVP ini secara eksplisit online-only dan offline bukan requirement sama sekali?

## 13. Observability & Maintenance
48. Setelah rilis 2 minggu, siapa yang akan maintain? Apakah orang yang sama yang develop, atau orang lain dengan level pengalaman yang berbeda?
49. Kebutuhan logging: cukup stdout dari docker-compose, atau perlu structured logging (JSON) yang bisa di-collect ke sistem log terpusat?
50. Kebutuhan monitoring: perlu dashboard dan alert untuk antrian redis, worker transcode mati, error rate naik, atau storage R2 menipis? Atau belum perlu?
51. Kalau terjadi insiden production, channel apa yang tersedia? Apakah ada on-call, atau cukup best effort?

## 14. Non-Functional Requirements
52. Ekspektasi latency API untuk feed, like, dan upload-intent? Misal p95 di bawah 300ms, atau tidak ada target khusus?
53. SLA transcode: dari upload confirm sampai status READY, berapa menit maksimal yang diterima? Misal video 60 detik harus selesai < 3 menit.
54. Target uptime MVP: 99%, 95%, atau tidak ada target karena masih tahap awal?
55. Seberapa penting arsitektur ini bisa diskalakan ke jutaan user? Apakah keputusan sekarang harus sudah mempertimbangkan skala besar, atau cukup benar untuk skala kecil dan bisa di-refactor nanti?

## 15. Edge Case & Kegagalan
56. Skenario kegagalan apa yang paling kamu takutkan? Misal: Zitadel down, R2 gagal upload, transcode stuck, atau PostgreSQL kehabisan disk.
57. Kalau Zitadel down, API harus menolak semua request, atau ada mode degraded tertentu?
58. Kalau transcode gagal, berapa kali retry yang diinginkan? Apakah user langsung diberi tahu, dan apakah video ditandai FAILED permanen?
59. Kalau presigned URL kadaluarsa, apakah user bisa minta URL baru? Bagaimana mencegah `r2_key` yang dikirim di endpoint confirm ternyata bukan milik user atau tidak valid?
60. Kalau user menghapus video yang sedang antri/diproses transcode, apa yang harus terjadi dengan job di Redis?
61. Untuk `views_count`: apakah satu user yang menonton video yang sama berkali-kali dihitung berkali-kali, atau perlu deduplication per user?
62. Kalau sebuah komentar punya balasan lalu komentar induk dihapus, apakah semua balasannya ikut terhapus (sesuai `ON DELETE CASCADE` di schema), atau perlu dipindahkan/dipertahankan?