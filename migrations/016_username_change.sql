-- =====================================================
-- 016_username_change.sql
-- Fitur B: penggantian username sendiri, maksimal sekali
-- per 7 hari.
--
-- CATATAN PENOMORAN: rencana awal menamai file ini
-- 009_username_change.sql, tetapi nomor 009 telah dipakai
-- oleh 009_dm_e2ee.sql di repo; nomor mengikuti urutan
-- migrasi yang ada (016 = nomor bebas berikutnya setelah
-- 015_dm_authorization_hardening.sql).
--
-- Migrasi ini ADDITIVE dan IDEMPOTEN, dibungkus
-- BEGIN/COMMIT agar ADD COLUMN dan CREATE UNIQUE INDEX
-- atomik: bila index gagal (mis. duplikat lower(username)),
-- SELURUH migrasi dibatalkan — tidak pernah ada kondisi
-- kolom ada tanpa index.
--
-- KEPUTUSAN GRANT (final): TIDAK ADA GRANT baru di file ini.
--   - tiktok_api (api-gateway) sudah memiliki
--     SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA
--     public dari migrations/001_init.sql, termasuk UPDATE
--     table-level yang otomatis mencakup kolom baru ini.
--   - tiktok_worker sengaja TIDAK boleh membaca kolom
--     username (pola column-level GRANT di
--     migrations/002_reconcile_users_grant.sql), jadi tidak
--     ada GRANT kolom apa pun untuk worker di sini.
--
-- ROLLBACK (opsional, operator manual):
--   DROP INDEX IF EXISTS uq_users_username_lower;
--   ALTER TABLE users DROP COLUMN IF EXISTS username_changed_at;
--   Risiko: timestamp cooldown hilang — semua user langsung
--   boleh ganti username lagi setelah rollback schema.
-- =====================================================

BEGIN;

-- 1) Kolom baru: NULL = belum pernah ganti username
--    (penggantian pertama langsung boleh).
ALTER TABLE users ADD COLUMN IF NOT EXISTS username_changed_at TIMESTAMPTZ;

-- 2) Keunikan case-insensitive. Kolom username hanya punya
--    UNIQUE biasa (migrations/001_init.sql: username TEXT
--    NOT NULL UNIQUE) yang masih membedakan 'Abc' vs 'abc'.
--    Index ini melengkapinya pada lower(username).
--    NOTE: CREATE INDEX IF NOT EXISTS memeriksa nama index,
--    bukan bentuknya — bila operator pernah membuat index
--    dengan nama lain pada lower(username), periksa manual.
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_username_lower ON users (lower(username));

COMMIT;

-- =====================================================
-- VERIFIKASI MANUAL OPERATOR (jalankan & laporkan hasilnya)
-- =====================================================
--
-- SEBELUM migrasi — cek duplikat lower(username). Bila
-- ada baris yang dikembalikan, BERHENTI: bersihkan
-- duplikat secara manual (jangan auto-rename; keputusan
-- pemilik), baru jalankan migrasi ini. Migrasi TIDAK
-- melakukan pengecekan runtime sendiri — atomisitas
-- BEGIN/COMMIT yang menjamin gagal-total:
--
--   SELECT lower(username), count(*) FROM users
--   GROUP BY lower(username) HAVING count(*) > 1;
--
-- SETELAH migrasi — kolom dan index harus terlihat:
--
--   \d users
--   -- kolom: username_changed_at | timestamp with time zone
--   -- index: uq_users_username_lower UNIQUE btree (lower(username))
--
-- SETELAH migrasi — bukti idempoten: jalankan file ini
-- DUA KALI dengan ON_ERROR_STOP=1; kedua kali harus sukses
-- tanpa error.
--
-- SETELAH migrasi — privilege tiktok_api (K12, tanpa GRANT
-- baru; hasil WAJIB t):
--
--   SELECT has_column_privilege('tiktok_api','users',
--          'username_changed_at','UPDATE');
--
-- SETELAH migrasi — kecepatan search (K13, verifikasi saja,
-- jangan ubah query SearchUsers kecuali dilaporkan):
--
--   EXPLAIN SELECT ... FROM users u ... ORDER BY LOWER(u.username) ...
--   -- laporkan apakah uq_users_username_lower dipakai.
