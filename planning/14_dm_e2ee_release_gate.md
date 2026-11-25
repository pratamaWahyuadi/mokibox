# Gate rilis E2EE DM

Dokumen ini memisahkan pekerjaan yang sudah terverifikasi di PR #82 dari keputusan yang harus diambil di luar repo. Merge PR tidak mengaktifkan fitur karena `DM_E2EE_ENABLED` default `false`.

## 1. Migrasi environment

Migrasi `009_dm_e2ee.sql`, `010_dm_inbox_sender_fk.sql`, dan `011_dm_fallback_keys.sql` wajib diterapkan di setiap environment sebelum binary baru menerima traffic. Ini berlaku walau flag DM mati, karena cleanup `DELETE /api/users/me` memanggil query DM di dalam transaksi yang sama.

Pastikan prasyarat `001`, `002`, dan `008` sudah terpasang. Untuk database yang belum punya DM:

```sh
make db-migrate-dm DB_NAME=<nama_db> COMPOSE="<perintah compose environment>"
```

Untuk database yang sudah punya `009` dari build sebelumnya:

```sh
make db-migrate-dm-existing DB_NAME=<nama_db> COMPOSE="<perintah compose environment>"
```

Verifikasi skema:

```sh
psql -h <host> -U <user> -d <nama_db> -Atqc "
  SELECT to_regclass('dm_devices') IS NOT NULL
     AND to_regclass('dm_fallback_keys') IS NOT NULL
     AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_dm_inbox_sender_user')
     AND NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_dm_inbox_sender_device')
     AND NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_dm_inbox_sender_user_id')"
```

Hasil harus `t`. Database lama yang masih menyimpan `idx_dm_inbox_sender_user_id` hanya mengalami drift index; hapus dengan `DROP INDEX IF EXISTS idx_dm_inbox_sender_user_id;` bila ingin rapi.

Status: terverifikasi di PostgreSQL CI dan smoke lokal. Belum dijalankan pada environment milik owner.

## 2. Keputusan produk

Perilaku v1 yang terdokumentasi adalah penghapusan sepihak: penghapusan akun menghapus pesan yang dikirim pengguna tersebut, sementara pesan yang diterimanya tetap tersimpan. Alternatif yang dipertimbangkan adalah menyimpan ciphertext pengguna yang dihapus.

Templat persetujuan:

- Owner produk:
- Keputusan: setuju dengan perilaku v1 / ubah ke alternatif retained ciphertext
- Alasan singkat:
- Tanggal:

Status: menunggu konfirmasi owner produk.

## 3. Lisensi dan audit

Implementasi memakai `maunium.net/go/mautrix` (MPL-2.0) dan `crypto/goolm` murni Go. Upstream menyatakan goolm masih membutuhkan audit; audit independen belum tersedia. Karena itu flag produksi tetap mati sampai kedua gate berikut terpenuhi:

- Legal menyetujui penggunaan ketergantungan MPL-2.0 dan kewajibannya.
- Security menerima hasil audit goolm atau memberikan penerimaan risiko tertulis.

Templat persetujuan:

- Approver legal:
- Approver security:
- Referensi audit / risiko:
- Tanggal:
- Keputusan mengaktifkan `DM_E2EE_ENABLED`: ya / tidak

Status: menunggu keputusan legal dan security.

## Ringkasan untuk reviewer

Kode, tes, race test, CI, smoke HTTP/PostgreSQL, dan dokumentasi sudah siap di PR #82. PR dapat di-merge tanpa mengaktifkan E2EE DM. Ketiga gate di atas tetap menjadi tanggung jawab owner environment, owner produk, dan legal/security; tidak ada yang dijadikan klaim selesai di luar bukti yang tersedia.
