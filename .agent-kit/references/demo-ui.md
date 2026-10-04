# Single-page browser demo UI untuk manual E2E (MokiBox phase-10)

Pattern: static HTML + CDN-only deps. Local: di-mount ke nginx MokiBox
lewat `docker-compose.override.yml` di path `/demo/`. Production VPS:
dilayani aaPanel vhost sebagai static alias `/demo/` (file sama di
host, tanpa container). Tujuan: tester manual E2E semua endpoint.

## Layout dasar

Single file `deploy/demo/index.html` dengan struktur:

- **Header**: badge auth state + tombol Login/Logout
- **Kolom aksi (kiri)**: cards per area (profil, upload, feed+detail, users+follow, notifikasi)
- **Log panel (kanan, sticky)**: live JSON request/response untuk debug
- **CSS**: Bootstrap 5 + hls.js via CDN, dark theme sederhana (no design overhead)
- **JS**: vanilla (zero build step), `api()` wrapper uniform yang inject
  `Authorization: Bearer <token>` dan log ke panel kanan

## Auth: PKCE S256 (pola mokibox_spa)

```
authorize → login Zitadel → /callback?code= → token exchange → UI
```

Yang biasanya dilupakan:
- **redirect_uri konstan** yang terdaftar di Zitadel app (`http://api.localhost/callback`).
  UI tidak boleh minta user registrasi redirect_uri baru setiap ganti UI.
- Nginx `location = /callback { return 302 /demo/$is_args$args; }` — kalau tidak,
  browser akan load `/callback?code=...` dari gateway backend dan dapat
  HTML 404 yang flash sesaat sebelum redirect manual. Lebih buruk: kalau
  `redirect_uri` mismatch Zitadel akan reject dengan `redirect_uri_mismatch`.
- **Token storage**: `sessionStorage` (cleared on tab close) — bukan
  `localStorage` (persistent). Untuk demo single-tab ini cukup; production
  flow punya refresh-token rotation.
- Pre-check `if(!logged())` dengan pesan eksplisit SEBELUM fetch — token
  hilang setelah container recreate / VPS reboot (sessionStorage reset).

## HLS player: auth via `?token=` URL, BUKAN Authorization header

**[REWRITTEN 2026-09-06 — section lama mengajarkan pola yang salah.]**
Pola lama "WAJIB auth header via xhrSetup/fetchSetup" adalah
**bug maker**: `xhrSetup` berlaku untuk SEMUA request hls.js,
termasuk fetch `.ts` segment ke R2 presigned URL. Presign
SigV4 hanya menandatangani header yang tercantum di
`X-Amz-SignedHeaders` (biasanya `host` saja) → header
`Authorization` ekstra = **400 SignatureDoesNotMatch**, dan
response error R2 tidak membawa CORS header → browser
menampilkannya sebagai **"CORS error"** yang menyesatkan
(ter-debug lintas 2 sesi sebelum ketemu). Detail kontrak
presign: skill `presigned-object-urls`.

**Pola yang benar (2-tier):**

1. **Playlist/manifest (origin api-gateway)**: pakai
   `v.hls_playlist_url` dari backend — sudah menyematkan
   `?token=<media-token>` (short-TTL, bound ke video). hls.js
   fetch URL itu **tanpa header custom**. Handler mendukung
   dua path: Bearer JWT ATAU `?token=` anonim.
2. **Segment `.ts` (origin R2)**: URL absolut presigned sudah
   ada di dalam playlist hasil rewrite backend. hls.js fetch
   headerless — JANGAN pernah sentuh konfigurasi header di
   hls.js untuk request ini.
3. **Route**: `/api/videos/:id/playlist.m3u8` harus bisa
   diakses anonim (di luar group auth ketat) — pakai
   `middleware.AuthenticateOptional`: header Authorization
   ABSENT → anonim (handler validasi `?token=`); PRESENT
   tapi invalid → fail-closed 401 (jangan silent downgrade,
   itu bypass token-path).

Konfigurasi hls.js: `new Hls({})` polos — tanpa `xhrSetup`,
tanpa `fetchSetup`, tanpa `Authorization` di mana pun.

Safari native HLS (`vid.src = playlistURL`) bekerja karena
token ada di query string, bukan di header.

Pelajaran: kalau endpoint media butuh auth, pilih mekanisme
yang **bisa hidup di URL** (query token), karena player dan
presigned storage sama-sama tidak bisa membawa custom header.

## Error handling UI: tunjukkan yang sebenarnya, jangan diam

Pattern buruk (jangan ulangi):
```js
} catch(e) { mark(0, 'err'); }  // mark(0, ...) mencari #s0 yg tidak ada
```
User melihat upload "stuck" tanpa tahu kenapa.

Pattern benar: tampilkan pesan konkret + tandai step yang gagal:
```js
} catch(e) {
  const msg = (e?.data?.error)
    ? `${e.status} ${e.data.error.code}: ${e.data.error.message}`
    : (typeof e === 'string' ? e : e?.message || 'error tak dikenal');
  $('#up-progress').html(`<span class="log-err">✗ gagal: ${esc(msg)}</span>`);
  log('err', 'upload gagal di langkah aktif — lihat status langkah (1/2/3)');
  [1,2,3].forEach(n => {
    const el = $('#s' + n);
    if (el && !el.classList.contains('log-ok')) el.classList.add('log-err');
  });
}
```

Prinsip: kalau smoke/tooling tidak menampilkan error konkret, user
tidak bisa debug. Silent failure 100% lebih buruk dari error jelek —
counterintuitive tapi lesson berulang (helper smoke != endpoint smoke,
ctx-expired silent fail, ProbeFile swallow error, dll).

## Asumsi yang harus diverifikasi live (bukan asumsi dari kode)

- `redirect_uri` persis terdaftar di Zitadel app
- `X-Forwarded-Proto/Host` di nginx agar Zitadel bangun issuer URL benar
  (untuk local: `X-Forwarded-Proto $scheme` saja sudah cukup di local.conf)
- SPA client_id: jangan hardcode di HTML — pakai `/demo/config.js`
  yang di-generate dari `.env` saat deploy (1-liner), dan add
  `deploy/demo/config.js` ke `.gitignore`
- Container publish port: kalau ada reverse proxy host (mis. aaPanel
  nginx sudah pegang :80/:443), mokibox-nginx container JANGAN publish
  port yang sama — local.conf atau production vhost yang jadi edge
- Skema URL (API / AUTH / REDIRECT_CB) JANGAN di-hardcode `http://` —
  derive dari `location.origin` / `location.protocol`. Hardcode http
  break prod https dua kali sekaligus (mixed-content block +
  redirect_uri scheme mismatch vs callback terdaftar); hardcode https
  break local smoke http. Satu file, dua environment.
- Instance Zitadel prod = SPA app baru → client ID BARU: regenerate
  `config.js` di server deploy dari `.env` prod; jangan pakai nilai
  client ID dev.

## Commit per class

`deploy/demo/index.html` + `docker-compose.override.yml` mount +
`deploy/nginx/local.conf` block — 1 commit, di-attach ke PR fase
yang relevan. File env-specific (`config.js`) TIDAK di-commit (gitignore).
Generator 1-liner masuk commit body agar reproducible:

```sh
printf 'window.SPA_CLIENT_ID = "%s";' \
  $(grep ^ZITADEL_SPA_CLIENT_ID= .env | cut -d= -f2-) \
  > deploy/demo/config.js
```
