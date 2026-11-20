---
name: zitadel
description: Use when working with ZITADEL v4 (any version) — Actions V2 targets/executions, OIDC apps, session v2 API, headless CLI flows, docker-compose deployment, webhook HMAC verification. Contains live-verified v4.16.0 gotchas (ROPC absent, Web-app-is-always-confidential, enum mappings, SSRF deny-list, aggregateID-vs-userID payload semantics).
---

# SKILL PACK INTEGRASI ZITADEL v4.16.0 & STACK PENDUKUNG

## Versi Dokumen: 3.0 — Berdasarkan Laporan Verifikasi Resmi

---

## 0. Context

Dokumen ini adalah **referensi teknis self-contained** untuk AI coding assistant yang bekerja dengan stack berikut:

| Komponen | Image / Versi |
|---|---|
| Zitadel | `v4.16.0` |
| Traefik | `traefik:v3.7.7` |
| PostgreSQL | `postgres:17.10-alpine` |
| Redis | `redis:7.4.9-alpine` |
| OpenTelemetry Collector | `otel/opentelemetry-collector-contrib:0.156.0` |

### Aturan Penggunaan Dokumen

1. **Gunakan dokumen ini sebagai sumber utama.** Jangan menambah asumsi teknis di luar isi dokumen ini saat menghasilkan kode atau konfigurasi.
2. **JANGAN melakukan scraping, fetching, atau akses internet** untuk melengkapi dokumen ini saat digunakan sebagai referensi kerja. Semua yang dibutuhkan sudah/harus ada di sini.
3. Bagian yang ditandai **`[TERKONFIRMASI]`** berarti telah diverifikasi terhadap dokumentasi resmi Zitadel dan siap pakai.
4. Bagian yang ditandai **`[PERLU VERIFIKASI]`** berarti detail tersebut **harus dicek ulang oleh manusia** terhadap dokumentasi resmi/source code Zitadel v4.16.0 sebelum dipakai di produksi.
5. Untuk hal yang benar-benar tidak diketahui (misalnya changelog literal v4.16.0), dokumen ini **tidak mengarang** — akan dinyatakan eksplisit "tidak tersedia dalam data training".

---

## 1. Arsitektur Umum Stack

```
Client (Browser/App)
   │  HTTPS
   ▼
Traefik v3.7.7  (reverse proxy, TLS termination, routing)
   │  HTTP/2 (h2c) internal
   ▼
Zitadel v4.16.0 (Auth Server: OIDC/OAuth2 + Management API + Actions V2)
   │                              │
   ▼                              ▼
PostgreSQL 17.10 (eventstore +   Redis 7.4.9
projections, primary datastore)  (app-level cache/session/rate-limit,
                                  BUKAN dependency wajib Zitadel core)
   │
   ▼
OpenTelemetry Collector 0.156.0 (traces/metrics dari Zitadel, Traefik, app)
```

**`[TERKONFIRMASI]`** Zitadel core (open-source) **tidak memiliki dependency resmi ke Redis** untuk fungsi intinya — datastore utamanya adalah PostgreSQL (event sourcing) atau CockroachDB. Jika Redis ada dalam stack ini, Redis dipakai oleh **layer aplikasi backend Anda** (cache token introspection, session store aplikasi, rate limiting di Traefik/API gateway), bukan oleh Zitadel itu sendiri.

---

## 2. Endpoint OAuth2 / OIDC `[TERKONFIRMASI]`

Zitadel mengekspos endpoint OIDC standar. Base URL di bawah menggunakan placeholder `{ZITADEL_DOMAIN}` (mis. `https://auth.contoh.com`).

### 2.1 Discovery Document

```
GET {ZITADEL_DOMAIN}/.well-known/openid-configuration
```

Selalu preferkan membaca discovery document ini **saat deployment** untuk memastikan path aktual sesuai instance yang berjalan.

### 2.2 Tabel Endpoint Utama `[TERKONFIRMASI]`

| Fungsi | Path | Method | Catatan |
|---|---|---|---|
| Authorization | `/oauth/v2/authorize` | GET | Redirect user untuk login |
| Token | `/oauth/v2/token` | POST | Tukar code/refresh_token/client_credentials jadi token |
| Token Introspection | `/oauth/v2/introspect` | POST | Validasi access/refresh token |
| Token Revocation | `/oauth/v2/revoke` | POST | Cabut token |
| Userinfo | `/oidc/v2/userinfo` | GET/POST | Ambil klaim identitas dari access token |
| JWKS | `/oauth/v2/keys` | GET | Public key untuk verifikasi JWT signature |
| End Session (Logout) | `/oidc/v2/end_session` | GET | RP-Initiated Logout |
| Device Authorization | `/oauth/v2/device_authorization` | POST | Device Code Flow |

### 2.3 Parameter Wajib per Endpoint `[TERKONFIRMASI]`

**Authorization Request (`/oauth/v2/authorize`):**

| Parameter | Wajib | Keterangan |
|---|---|---|
| `client_id` | Ya | ID aplikasi OIDC yang terdaftar |
| `redirect_uri` | Ya | Harus persis cocok dengan yang terdaftar di aplikasi |
| `response_type` | Ya | `code` (Authorization Code), `id_token`, dll |
| `scope` | Ya | Minimal `openid`. Bisa tambah `profile`, `email`, `offline_access` |
| `state` | Disarankan | CSRF protection |
| `code_challenge` | Wajib untuk PKCE | Base64URL(SHA256(code_verifier)) |
| `code_challenge_method` | Wajib untuk PKCE | `S256` |
| `nonce` | Disarankan (OIDC) | Mencegah replay pada id_token |

**Token Request (`/oauth/v2/token`):**

| Grant Type | Parameter Wajib |
|---|---|
| `authorization_code` | `grant_type=authorization_code`, `code`, `redirect_uri`, `client_id`, `code_verifier` (jika PKCE) |
| `client_credentials` | `grant_type=client_credentials`, `client_id`, `client_secret`, `scope` |
| `refresh_token` | `grant_type=refresh_token`, `refresh_token`, `client_id` |
| `urn:ietf:params:oauth:grant-type:jwt-bearer` | `grant_type`, `assertion` (signed JWT) |
| `urn:ietf:params:oauth:grant-type:device_code` | `grant_type`, `device_code`, `client_id` |

### 2.4 Alur Autentikasi yang Didukung `[TERKONFIRMASI]`

1. **Authorization Code + PKCE** — direkomendasikan untuk SPA & mobile app.
2. **Authorization Code (confidential client)** — untuk server-side web app.
3. **Client Credentials** — machine-to-machine (service account).
4. **JWT Profile (Private Key JWT)** — service account tanpa share secret.
5. **Device Authorization Grant** — untuk perangkat tanpa browser.
6. **Refresh Token Grant** — perpanjang access token.

**Tidak didukung / dihindari:**
- **Implicit Flow** — tidak disarankan (OAuth 2.1 best practice).
- **Resource Owner Password Credentials (ROPC)** — **TIDAK ADA di Zitadel v4, sama sekali.** Bukan sekadar "tidak disarankan": token endpoint menjawab `{"error":"unsupported_grant_type","error_description":"password not supported"}` dan tidak ada setting instance/app untuk mengaktifkannya. **[TERVERIFIKASI LIVE v4.16.0 — MokiBox fase 10]** Jangan rancang test script / CLI dengan asumsi `grant_type=password`; untuk headless login lihat section "Headless CLI Flows (Live-Verified)" di bawah.

---

## 3. Struktur Organisasi & Resource Model Zitadel `[TERKONFIRMASI]`

Hierarki resource Zitadel (top-down):

```
Instance (satu deployment Zitadel)
 └─ Organization (org) — unit tenant/perusahaan
     ├─ Project — kumpulan aplikasi yang berbagi role & authorization
     │   ├─ Application (OIDC / API / SAML)
     │   ├─ Role — didefinisikan di level Project
     │   └─ Project Grant — memberi akses project ini ke org lain
     ├─ User — Human User atau Machine/Service User
     │   └─ User Grant — pemberian role tertentu ke user tertentu
     └─ IDP (Identity Provider) — konfigurasi login eksternal
```

### 3.1 Penjelasan Entitas

| Entitas | Deskripsi |
|---|---|
| **Instance** | Root tenant Zitadel; bisa multi-instance dalam satu deployment. |
| **Organization** | Batas isolasi utama untuk data (user, project, IDP) dalam satu instance. |
| **Project** | Wadah untuk 1+ Application yang berbagi definisi Role. |
| **Application** | Client OAuth2/OIDC konkret: Web, Native, User Agent (SPA), atau API. |
| **User** | `Human User` atau `Machine/Service User`. |
| **Role** | Didefinisikan di Project, berupa `role_key` unik. |
| **User Grant** | Relasi `User ↔ Project (+Role)` yang menentukan hak akses efektif. |
| **Project Grant** | Relasi antar-organization untuk berbagi akses project. |

### 3.2 Klaim Custom di Token

Role/authorization user biasanya muncul di token sebagai custom claim:

```json
"urn:zitadel:iam:org:project:roles": {
  "role_key_1": {
    "orgId_1": "org_name_1"
  }
}
```

---

## 4. Manajemen User via REST API `[TERKONFIRMASI]`

Zitadel memiliki dua generasi API REST:

- **Legacy v1 API** (`/management/v1/*`, `/auth/v1/*`, `/admin/v1/*`) — pola RPC-style.
- **Resource-based v2 API** (`/v2/*`) — REST yang lebih konvensional.

### 4.1 Legacy Management API v1

| Aksi | Method & Path |
|---|---|
| Buat human user | `POST /management/v1/users/human` |
| Buat machine user | `POST /management/v1/users/machine` |
| Update profile user | `PUT /management/v1/users/{userId}/profile` |
| Get user by ID | `GET /management/v1/users/{userId}` |
| List users | `POST /management/v1/users/_search` |
| Deactivate user | `POST /management/v1/users/{userId}/_deactivate` |
| Hapus user | `DELETE /management/v1/users/{userId}` |
| Set password | `POST /management/v1/users/{userId}/password` |
| Tambah User Grant | `POST /management/v1/users/{userId}/grants` |

### 4.2 Resource API v2 `[PERLU VERIFIKASI]`

| Aksi | Method & Path |
|---|---|
| Buat human user | `POST /v2/users/human` |
| Update user | `PATCH /v2/users/{userId}` |
| Get user | `GET /v2/users/{userId}` |
| List users | `POST /v2/users/search` |
| Deactivate | `POST /v2/users/{userId}/deactivate` |
| Hapus | `DELETE /v2/users/{userId}` |

### 4.3 Autentikasi ke Management API `[TERKONFIRMASI]`

Semua panggilan Management/Auth/Admin API butuh **access token** (Bearer) dari user/service account yang punya izin:

```
Authorization: Bearer {access_token}
x-zitadel-orgid: {orgId}   // opsional, override org context
```

---

## 5. Actions V2 (WAJIB — V1 Deprecated) `[TERKONFIRMASI]`

### 5.1 Kenapa V1 Tidak Boleh Dipakai

| Aspek | Actions V1 (Deprecated) | Actions V2 (Gunakan Ini) |
|---|---|---|
| Mekanisme | JavaScript dieksekusi **di dalam** proses Zitadel | HTTP webhook ke **Target** eksternal |
| Model Trigger | "Flow & Trigger" tetap | "Execution" fleksibel: Request, Response, Function, atau Event |
| Keamanan | Kode JS arbitrary dalam proses auth server | Logic dieksekusi di luar Zitadel dengan signature HMAC |
| Status | **Deprecated**, dijadwalkan dihapus di V5 | Model resmi & didukung aktif |

### 5.2 Konsep Inti Actions V2 `[TERKONFIRMASI]`

- **Target**: endpoint eksternal (URL) yang akan dipanggil Zitadel via HTTP POST, beserta:
  - `endpoint` (URL tujuan)
  - `timeout` (batas waktu tunggu respons)
  - `signingKey` (secret untuk HMAC — di-generate Zitadel)
  - **Mode Target** (3 tipe):
    - **`restWebhook`** (async): `"restWebhook": { "interruptOnError": false }` — fire-and-forget
    - **`restCall`** (sync/interceptor): `"restCall": { "interruptOnError": true }` — respons dapat mengubah alur
    - **`restAsync`**: panggilan paralel tanpa menunggu respons (tipe ketiga yang terkonfirmasi)
- **Execution**: aturan yang menghubungkan sebuah **Condition** ke satu atau lebih **Target**.
- **Condition**, salah satu dari:
  1. **Request** — sebelum method API tertentu dieksekusi (pakai gRPC method path)
  2. **Response** — setelah method API tertentu dieksekusi (pakai gRPC method path)
  3. **Function** — titik ekstensi spesifik dalam flow internal (**case-sensitive, lowercase!**)
  4. **Event** — dipicu oleh event tertentu di eventstore Zitadel

### 5.3 Penamaan Condition Function — **WAJIB LOWERCASE** `[TERKONFIRMASI]`

**⚠️ KRITIS:** Nama function dalam kondisi Execution **harus menggunakan lowercase** (case-sensitive). Penggunaan CamelCase (mis. `PreUserinfo`) akan menyebabkan webhook tidak pernah dipicu (silent failure).

| Function Name (BENAR) | Deskripsi |
|---|---|
| `preaccesstoken` | Dipanggil sebelum token dibuat; dapat menambahkan custom claims |
| `preuserinfo` | Dipanggil sebelum userinfo response dikirim |
| `presamlresponse` | Dipanggil sebelum SAML response dibuat |

**`[PERLU VERIFIKASI]`** — Daftar function names di atas adalah yang terkonfirmasi. Function lain seperti `postauthentication` perlu diverifikasi terhadap dokumentasi resmi v4.16.0.

### 5.4 Struktur Payload Webhook (Umum) `[TERKONFIRMASI + LIVE-VERIFIED v4.16.0]`

**⚠️ KRITIS — `userID` vs `aggregateID` (ditemukan live di MokiBox fase 10):**

Payload Actions V2 membawa DUA id dengan makna berbeda:

| Field | Makna | Contoh nilai saat admin men-deactivate user B
|---|---|---|
| `aggregateID` | **User/agregat yang TERDAMPAK** (target event) | ID user B |
| `userID` | **AKTOR yang memicu event** | ID admin yang memanggil API |

Untuk event self-actored (mis. `user.human.added` saat registrasi mandiri) keduanya sama — karena it semua contoh dokumen menunjukkan `userID == aggregateID` dan terlihat interchangeable. **Mereka TIDAK interchangeable.** Webhook handler yang dispatch di `userID` akan men-tombstone/hapus **akun admin** (bukan user target) saat admin memproses deactivation/removal user lain. **Selalu dispatch di `aggregateID`** (fallback `userID` hanya untuk payload lawas tanpa aggregateID).

Payload live yang dibuktikan (v4.16.0, event user.deactivated terhadap user `388995719870021635` oleh admin):

```json
{
  "aggregateID": "388995719870021635",
  "aggregateType": "user",
  "resourceOwner": "388988190574313475",
  "instanceID": "388988190574247939",
  "version": "v2",
  "sequence": 10,
  "event_type": "user.deactivated",
  "created_at": "2026-09-02T23:14:57.451335Z",
  "userID": "388988190574837763"
}
```

Catatan: field `payload` dari contoh lama TIDAK muncul di event `user.deactivated` v4.16.0 — jangan mengandalkan `payload.userName` untuk lookup; gunakan `aggregateID`.

Contoh payload self-actored (konteks lengkap, dari verifikasi resmi):

```json
{
  "aggregateID": "223104242408259876",
  "aggregateType": "user",
  "orgID": "163840776835432193",
  "instanceID": "163840776835432000",
  "eventType": "user.human.added",
  "createdAt": "2026-08-26T09:15:00Z",
  "sequence": 4821,
  "userID": "223104242408259876",
  "payload": {
    "userName": "budi.santoso",
    "firstName": "Budi",
    "lastName": "Santoso",
    "email": "budi.santoso@contoh.com",
    "isEmailVerified": false
  }
}
```

(Terlihat dua varian penamaan field antar versi — `eventType`/`createdAt`/`orgID` vs `event_type`/`created_at`/`resourceOwner`. Di v4.16.0 live payload memakai snake_case `event_type` + `created_at` + `resourceOwner`. Handler sebaiknya tolerant terhadap keduanya.)

### 5.5 Verifikasi Signature HMAC — **DUAL HEADER** `[TERKONFIRMASI]`

**⚠️ KRITIS:** Dokumentasi Zitadel memiliki dua varian header signature. Handler WAJIB memeriksa kedua varian:

| Header | Sumber |
|---|---|
| `ZITADEL-Signature` | Panduan integrasi (Guides & Tutorials) |
| `X-ZITADEL-Signature` | Spesifikasi gRPC/OpenAPI |

**Format:** `t=<timestamp>,v1=<hmac_hex>`

**Algoritma:** HMAC-SHA256, dihitung atas **`${timestamp}.${rawBody}`** (timestamp + dot + raw request body).

**Kode Verifikasi (DENGAN DUAL HEADER):**

```go
package actionsverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ZitadelActionResponse mendefinisikan struktur JSON balasan sinkron
// untuk Target bertipe restCall.
type ZitadelActionResponse struct {
	SetUserMetadata []UserMetadata `json:"set_user_metadata,omitempty"`
	AppendClaims    []CustomClaim  `json:"append_claims,omitempty"`
	AppendLogClaims []string       `json:"append_log_claims,omitempty"`
}

type UserMetadata struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

type CustomClaim struct {
	Key   string      `json:"key"`
	Value interface{} `json:"value"`
}

// VerifyZitadelSignature memverifikasi HMAC signature dengan dukungan dual-header.
func VerifyZitadelSignature(r *http.Request, signingKey string, tolerance time.Duration) ([]byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("gagal membaca body: %w", err)
	}

	// DUAL HEADER: cek kedua varian
	sigHeader := r.Header.Get("ZITADEL-Signature")
	if sigHeader == "" {
		sigHeader = r.Header.Get("X-ZITADEL-Signature")
	}
	if sigHeader == "" {
		return nil, errors.New("missing ZITADEL-Signature or X-ZITADEL-Signature header")
	}

	// Parse format: t=<timestamp>,v1=<hmac_hex>
	var ts int64
	var sig string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts, _ = strconv.ParseInt(kv[1], 10, 64)
		case "v1":
			sig = kv[1]
		}
	}
	if ts == 0 || sig == "" {
		return nil, errors.New("malformed signature header: missing t or v1")
	}

	// Cek timestamp tolerance (rekomendasi 5 menit)
	if tolerance > 0 && time.Since(time.Unix(ts, 0)) > tolerance {
		return nil, errors.New("signature timestamp outside tolerance")
	}

	// Hitung HMAC dari timestamp + "." + raw body
	signedPayload := fmt.Sprintf("%d.%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return nil, errors.New("invalid signature")
	}

	return body, nil
}
```

### 5.6 Konfigurasi Actions V2 (via Management API) `[TERKONFIRMASI]`

**1. Buat Target (3 tipe):**

**Async Webhook (fire-and-forget):**
```json
POST /v2/actions/targets
{
  "name": "audit-logger",
  "restWebhook": {
    "interruptOnError": false
  },
  "endpoint": "https://backend.internal/webhooks/audit",
  "timeout": "10s"
}
```

**Sync Call (interceptor):**
```json
POST /v2/actions/targets
{
  "name": "claim-enricher",
  "restCall": {
    "interruptOnError": true
  },
  "endpoint": "https://backend.internal/webhooks/claims",
  "timeout": "5s"
}
```

**Async Call (paralel tanpa menunggu):**
```json
POST /v2/actions/targets
{
  "name": "notification-sender",
  "restAsync": {
    "interruptOnError": false
  },
  "endpoint": "https://backend.internal/webhooks/notify",
  "timeout": "10s"
}
```

Response akan berisi `signingKey` — **simpan sebagai secret** di Vault/Secret Manager.

**2. Buat Execution:**

**Untuk Event Condition (v4.16.0 — LIVE-VERIFIED 2026-09-09, MokiBox domain-migration):**
```json
PUT /v2/actions/executions
{
  "condition": {"event": {"event": "user.deactivated"}},
  "targets": ["<targetID_returned>"]
}
```
**⚠️ v4.16 payload shape berbeda dari contoh lama**: field di dalam
`condition.event` adalah **`event`** (string), BUKAN `eventType`.
Diverifikasi dari proto source tag v4.16.0 (`EventExecution` punya
oneof `condition` dengan field `string event = 1`). Payload
`{"event":{"eventType":"..."}}` ditolak dengan
`invalid EventExecution.Condition: value is required`.
Contoh lama dengan `eventType` berlaku untuk versi lebih awal — cek
shape via proto source untuk versi target sebelum provisioning.

**Untuk Function Condition — GUNAKAN LOWERCASE:**
```json
PUT /v2/actions/executions
{
  "condition": {
    "function": { "name": "preaccesstoken" }
  },
  "targets": ["<targetID_returned>"]
}
```

**Untuk Response Condition:**
```json
PUT /v2/actions/executions
{
  "condition": {
    "response": {
      "method": "/zitadel.user.v2.UserService/RetrieveIdentityProviderIntent"
    }
  },
  "targets": ["<targetID_returned>"]
}
```

**Untuk Request Condition:**
```json
PUT /v2/actions/executions
{
  "condition": {
    "request": {
      "method": "/zitadel.user.v2.UserService/CreateUser"
    }
  },
  "targets": ["<targetID_returned>"]
}
```

**Granulasi Event Condition `[TERKONFIRMASI]`:**

Zitadel mendukung tiga tingkatan pemicuan event:

| Level | Contoh | Deskripsi |
|---|---|---|
| **Event** | `{"eventType": "user.human.added"}` | Pemicuan pada event spesifik |
| **Group** | `{"eventGroup": "user"}` | Pemicuan pada seluruh event dalam satu kelompok agregat |
| **All** | `{"all": {}}` | Pemicuan pada seluruh event di eventstore |

### 5.7 Contoh Handler Actions V2 — DENGAN FIELD RESPONSE YANG BENAR

#### (a) Custom Claims Saat Token Issuance — **DENGAN FIELD YANG BENAR** `[TERKONFIRMASI]`

**⚠️ KRITIS:** Nama field respons adalah **`append_claims`** (snake_case), bukan `appendClaims` (camelCase). Menggunakan field yang salah akan menyebabkan custom claims diam-diam tidak muncul di token (silent failure).

**⚠️ KRITIS:** Function name yang digunakan adalah **`preaccesstoken`** (lowercase), bukan `PreTokenCreation`.

```go
func HandleCustomClaims(signingKey string, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := VerifyZitadelSignature(r, signingKey, 5*time.Minute)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		
		var req struct {
			UserID string `json:"userID"`
		}
		_ = json.Unmarshal(body, &req)

		var department, employeeID string
		_ = db.QueryRow(`SELECT department, employee_id FROM app_users WHERE zitadel_user_id = $1`,
			req.UserID).Scan(&department, &employeeID)

		// FIELD YANG BENAR: append_claims (snake_case)
		// GUNAKAN: ZitadelActionResponse struct dari atas
		resp := ZitadelActionResponse{
			AppendClaims: []CustomClaim{
				{Key: "department", Value: department},
				{Key: "employee_id", Value: employeeID},
			},
			AppendLogClaims: []string{
				fmt.Sprintf("Enriched token for user %s via Actions V2", req.UserID),
			},
		}
		
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
```

#### (b) Sinkronisasi User ke Database Lokal

```go
func HandleUserSyncWebhook(signingKey string, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := VerifyZitadelSignature(r, signingKey, 5*time.Minute)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var evt struct {
			EventType string `json:"eventType"`
			UserID    string `json:"userID"`
			OrgID     string `json:"orgID"`
			Payload   struct {
				UserName string `json:"userName"`
				Email    string `json:"email"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(body, &evt); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}

		switch evt.EventType {
		case "user.human.added":
			_, err = db.Exec(`
				INSERT INTO app_users (zitadel_user_id, org_id, username, email)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (zitadel_user_id) DO UPDATE
				SET username = EXCLUDED.username, email = EXCLUDED.email`,
				evt.UserID, evt.OrgID, evt.Payload.UserName, evt.Payload.Email)
		case "user.removed":
			_, err = db.Exec(`DELETE FROM app_users WHERE zitadel_user_id = $1`, evt.UserID)
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
```

#### (c) Handler untuk Function `preuserinfo`

```go
func HandlePreUserinfo(signingKey string, userService UserService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := VerifyZitadelSignature(r, signingKey, 5*time.Minute)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			UserID string `json:"userID"`
		}
		_ = json.Unmarshal(body, &req)

		// Ambil data tambahan dari database
		userData, _ := userService.GetExtraUserData(req.UserID)

		resp := ZitadelActionResponse{
			AppendClaims: []CustomClaim{
				{Key: "user_tier", Value: userData.Tier},
				{Key: "custom_permissions", Value: userData.Permissions},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
```

### 5.8 WAJIB Test End-to-End Sebelum Production

**⚠️ KRITIS:** Karena signature verification dan custom claims adalah area yang paling mungkin menyebabkan **security-silent-fail**, implementasi Actions V2 **WAJIB diuji end-to-end** pada environment staging dengan:
1. Verifikasi signature berhasil dengan dual-header (`ZITADEL-Signature` atau `X-ZITADEL-Signature`).
2. Custom claims muncul di token yang dihasilkan (cek `id_token` atau `userinfo` endpoint).
3. Response time tidak melebihi `timeout` yang dikonfigurasi di Target.

---

## 5.9 Headless CLI Flows + OIDC App Gotchas (LIVE-VERIFIED v4.16.0 — MokiBox fase 10)

Section ini adalah hasil trial-and-error nyata; semua terverifikasi terhadap Zitadel v4.16.0 docker-compose (domain auth.localhost, no TLS).

### 5.9.1 Headless login HUMAN user (tanpa browser, tanpa ROPC)

Karena ROPC tidak ada, satu-satunya jalur headless untuk user manusia = authorization_code flow dengan session v2 API (pola ini persis yang dipakai Login UI):

```
1. GET  /oauth/v2/authorize?client_id=...&response_type=code&scope=openid+profile
        &redirect_uri=<registered>&state=...
   → 302; Location: /ui/v2/login/login?authRequest=V2_<ID>   (extract V2_<ID>)

2. POST /v2/sessions   (Authorization: Bearer <LOGIN_CLIENT_PAT>)
   body: {"checks":{"user":{"loginName":"<user>"},"password":{"password":"<pass>"}}}
   → {"sessionId":"...","sessionToken":"..."}
   CATATAN: Bearer WAJIB PAT machine user `login-client` (dari bootstrap volume,
   path /zitadel/bootstrap/login-client.pat saat start-from-init). PAT user biasa
   atau tanpa auth → 401. Human user TIDAK BISA generate PAT di console — hanya machine user.

3. POST /v2/oidc/auth_requests/V2_<ID>   (Authorization: Bearer <LOGIN_CLIENT_PAT>)
   body: {"session":{"sessionId":"<sessionId>","sessionToken":"<sessionToken>"}}
   → {"callbackUrl":"<redirect_uri>?code=<AUTH_CODE>&state=..."}
   Endpoint ini dipakai INTERNAL Login V2; otentikasi HARUS login-client PAT
   (token user → "membership not found (AUTHZ)"; session-token → "membership not found").

4. POST /oauth/v2/token  (client auth sesuai tipe app — lihat 5.9.2)
   body: grant_type=authorization_code&code=<CODE>&redirect_uri=<registered>[&client_secret=...]
   → access_token (JWT bila accessTokenType app = JWT)
```

Referensi implementasi lengkap: fungsi `headless_login()` di `MokiBox/scripts/integration_test.sh`.

### 5.9.2 OIDC app types — WEB SELALU CONFIDENTIAL (jebakan terbesar)

**App tipe Web (application_type=0) SELALU diperlakukan confidential di Zitadel v4** — token endpoint menolak PKCE tanpa secret: `{"error":"invalid_client","error_description":"empty client secret"}` WALAUI authMethodType=NONE (public) tercatat di DB. Console tetap menawarkan opsi NONE saat create Web app (menyesatkan).

PKCE murni tanpa secret HANYA untuk tipe **User Agent / SPA (application_type=1)** atau **Native (=2)**. Pola Management Console Zitadel sendiri: SPA + NONE + PKCE.

Enum v1 (AddOIDCApp/UpdateOIDCAppConfig) — HAFALKAN, ini tidak intuitif:

| Field | Nilai |
|---|---|
| appType | 0=Web, 1=UserAgent(SPA), 2=Native |
| authMethodType | **0=BASIC, 1=POST, 2=NONE** (0 BUKAN NONE!) |
| accessTokenType | 0=Bearer(opaque), 1=JWT |
| responseTypes/grantTypes | 0=code/authorization_code |

Gotcha lain yang terbukti:
- **application_type TIDAK BISA diubah setelah create** (UpdateApp mengabaikannya; "No changes"). Salah tipe → delete + recreate app.
- **PUT /oidc_config dengan subset field MENIMPA field lain ke default proto3** — kirim `accessTokenType` saja → authMethodType ikut kembali ke 0. Selalu kirim SEMUA field relevan (clientId, authMethodType, accessTokenType, ...) dalam satu PUT. Path benar: `/management/v1/projects/{pid}/apps/{aid}/oidc_config` (underscore, bukan `oidc/config`).
- Web app tanpa secret yang lupa digenerate → 400 di token; generate via `POST .../oidc_config/_generate_client_secret`.
- **devMode (isDevMode=true) wajib** untuk redirect_uri http:// non-localhost-port di dev; tanpa itu authorize gagal.
- Access token default = **opaque/JWE** (bukan JWT). Resource server yang verify JWT lokal WAJIB set `accessTokenType=1` per app — kalau tidak, verifier JWT selalu gagal walau signature ok.
- loginName admin first-instance = `zitadel-admin@zitadel.<externaldomain>` (bukan @zitadel.localhost!) — cek via `projections.login_names3_users` bila ragu; default password `Password1!` saat start-from-init.

### 5.9.3 Actions V2 Target = SSRF deny-list (kenapa "DeniedURL")

Membuat target dengan endpoint ke network internal (mis. http://api.localhost yang resolve ke 172.x Docker) gagal:
`{"code":3,"message":"Errors.Target.DeniedURL"}` dengan log `err.parent="address is denied by '172.16.0.0/12'"`.

Penyebab: default config `HTTPClient.DenyList` (env `ZITADEL_HTTPCLIENT_DENYLIST`, comma-separated) memblokir SEMUA CIDR private + loopback + link-local + metadata (10/8, 172.16/12, 192.168/16, 127/8, 169.254/16, ::1, fe80::/10, dll).

Untuk DEV SAJA: override env di container, mis. `ZITADEL_HTTPCLIENT_DENYLIST: "localhost,127.0.0.0/8,169.254.0.0/16,::1/128,fe80::/10"` (pertahankan loopback+metadata, izinkan bridge Docker). **JANGAN pakai di production** — guard ini mencegah SSRF. Contoh file: `MokiBox/scripts/zitadel-override.example.yml`.

SetExecution (PUT) MENIMPA daftar target untuk condition itu — menambah target kedua harus PUT ulang dengan ARRAY target lengkap, bukan append.

### 5.9.6 User search + set password + management auth (LIVE-VERIFIED v4.16.0, 2026-09-10 prod VPS)

- **User search**: `POST /v2/users` (BUKAN `/v2/users/search` — itu 405 Method Not Allowed). Body `{"queries":[{"userNameQuery":{"userName":"test","method":"TEXT_QUERY_METHOD_STARTS_WITH"}}]}`. Result rows pakai field `userId` (bukan `id`) + `username` + `state`.
- **Set/reset password user existing**: `POST /v2/users/{userId}/password` body `{"password":{"password":"...","change_required":false}}` (PUT = 405; `/v2beta/.../password` = 405).
- **Management API auth**: login-client PAT **TIDAK BISA** call management/v1 atau v2 actions (403 `No matching permissions found (AUTH-5mWD2)`). Yang bekerja (live-verified prod): create session admin via `POST /v2/sessions` (Bearer = login-client PAT, body loginName `zitadel-admin@zitadel.<externaldomain>` + password) → pakai **sessionToken** sebagai Bearer untuk management/v1 + v2 actions + v2 users (pola sama dengan integration_test.sh step 10/12).

### 5.9.4 Create human user + password dalam SATU call (v4.16.0 — live-verified 2026-09-09)

`POST /management/v1/users/human` (v1) **MENGABAIKAN field `password`**
di body — user dibuat state 6 (initial/password-change-required),
dan `POST /management/v1/users/{id}/password` kemudian ditolak
dengan `{"code":9,"message":"User is not yet initialized (COMMAND-M9dse)"}`
selama user belum pernah login/initialized. Juga: field nama di v1
adalah `profile` (bukan `name`) — body tanpa `profile` ditolak
`invalid AddHumanUserRequest.Profile: value is required`.

Pola yang benar (v2beta, satu call, password langsung aktif):
```
POST /v2beta/users/human
{
  "username": "test1",
  "profile": {"given_name": "test1", "family_name": "MokiBox"},
  "email": {"email": "test1@example.local", "is_verified": true},
  "password": {"password": "...", "change_required": false}
}
```
Catatan: endpoint-nya `POST /v2beta/users/human` (bukan
`/v2beta/users` — yang itu adalah list/search).

### 5.9.5 Cara mengintip payload webhook (debug technique)

Ketika perlu membuktikan bentuk payload asli: daftarkan target kedua sementara yang menunjuk ke listener di network Zitadel (socat dalam container alpine: `socat -T 5 TCP-LISTEN:8899,reuseaddr,fork SYSTEM:'echo "HTTP/1.1 200 OK"; echo "Content-Length: 2"; echo; echo "{}"; cat >> /logs/hook.log'` — busybox `nc -l` TIDAK cukup: EOF tanpa respons → Zitadel log "error calling target ... EOF" dan retry). Setelah capture, PUT ulang execution dengan target asli saja + hapus listener.

## 6. Integrasi Traefik (v3.7.7) `[TERKONFIRMASI]`

### 6.1 Header yang Wajib Diteruskan ke Zitadel

| Header | Kegunaan |
|---|---|
| `X-Forwarded-Proto` | Agar Zitadel tahu request asli `https`, penting untuk redirect URI & cookie `Secure` |
| `X-Forwarded-Host` | Agar Zitadel membangun URL/issuer yang benar sesuai domain publik |
| `X-Forwarded-For` | IP client asli untuk audit log & rate limiting |
| `Host` | Harus sama dengan domain yang dikonfigurasi sebagai `ExternalDomain` di Zitadel |

### 6.2 Contoh Konfigurasi Traefik (Dynamic Config, Docker Labels)

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.zitadel.rule=Host(`auth.contoh.com`)"
  - "traefik.http.routers.zitadel.entrypoints=websecure"
  - "traefik.http.routers.zitadel.tls=true"
  - "traefik.http.routers.zitadel.tls.certresolver=letsencrypt"
  - "traefik.http.services.zitadel.loadbalancer.server.port=8080"
  # WAJIB: Zitadel mendukung gRPC-Web & HTTP/2; aktifkan h2c
  - "traefik.http.services.zitadel.loadbalancer.server.scheme=h2c"
  - "traefik.http.middlewares.zitadel-headers.headers.customrequestheaders.X-Forwarded-Proto=https"
  - "traefik.http.routers.zitadel.middlewares=zitadel-headers"
```

**Penting:** Penggunaan skema `h2c` (HTTP/2 Cleartext) **wajib** untuk komunikasi internal Traefik→Zitadel. Jika menggunakan HTTP/1.1, akan terjadi error `HTTP 505 HTTP Version Not Supported` pada Admin Console.

### 6.3 TLS `[TERKONFIRMASI]`

- Terminasi TLS publik di Traefik (Let's Encrypt atau sertifikat custom).
- Zitadel perlu tahu bahwa ia berada di belakang proxy TLS — pastikan env `ZITADEL_EXTERNALSECURE=true` dan `ZITADEL_EXTERNALDOMAIN` sesuai domain publik.

---

## 7. Integrasi PostgreSQL (v17.10-alpine) `[TERKONFIRMASI]`

### 7.1 Peran PostgreSQL

Zitadel memakai model **Event Sourcing + CQRS**:
- **Eventstore**: tabel append-only yang mencatat setiap perubahan state sebagai event.
- **Projections**: tabel-tabel read-model yang di-generate dari event untuk query cepat.

### 7.2 Catatan Migrasi

- Zitadel menjalankan migrasi skema database **secara otomatis** saat startup.
- **Penting saat upgrade**: selalu backup database sebelum upgrade.
- Gunakan role PostgreSQL terpisah dengan privilege minimal, jangan pakai superuser untuk runtime.
- Pastikan `max_connections` PostgreSQL cukup untuk connection pool Zitadel.

### 7.3 Environment Variable Koneksi `[TERKONFIRMASI]`

```env
ZITADEL_DATABASE_POSTGRES_HOST=postgres
ZITADEL_DATABASE_POSTGRES_PORT=5432
ZITADEL_DATABASE_POSTGRES_DATABASE=zitadel
ZITADEL_DATABASE_POSTGRES_USER_USERNAME=zitadel
ZITADEL_DATABASE_POSTGRES_USER_PASSWORD=__secret__
ZITADEL_DATABASE_POSTGRES_USER_SSL_MODE=require
ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME=postgres
ZITADEL_DATABASE_POSTGRES_ADMIN_PASSWORD=__secret__
ZITADEL_DATABASE_POSTGRES_ADMIN_SSL_MODE=require
```

**`[PERLU VERIFIKASI]`** — Zitadel mungkin juga mendukung DSN connection string sebagai alternatif; periksa release notes v4.16.0.

---

## 8. Integrasi Redis (v7.4.9-alpine) `[TERKONFIRMASI]`

**Catatan:** Zitadel core **tidak punya integrasi resmi bawaan** dengan Redis. Penggunaannya berada di **application layer**:

| Use Case | Key Pattern yang Disarankan | TTL |
|---|---|---|
| Cache token introspection | `zitadel:introspect:{token_hash}` | ≤ 60–300s |
| Cache JWKS (public key) | `zitadel:jwks:{kid}` | 1–24 jam |
| Session aplikasi | `app:session:{session_id}` | Sesuai kebijakan |
| Rate limiting | `ratelimit:login:{ip_or_user}` | Sliding window, 60s |
| Idempotency key webhook | `zitadel:webhook:seen:{aggID}:{seq}` | 24 jam |

**Penting:** JANGAN menyimpan token mentah di Redis tanpa enkripsi.

---

## 9. Integrasi OpenTelemetry Collector (v0.156.0) `[PERLU VERIFIKASI]`

### 9.1 Konsep

Zitadel memiliki dukungan tracing & metrics via OpenTelemetry (OTEL), yang diarahkan ke OpenTelemetry Collector via OTLP (gRPC atau HTTP).

**`[TERKONFIRMASI]`** — Zitadel memancarkan distributed tracing yang mencakup seluruh alur autentikasi dan kueri eventstore. Ketika Traefik v3.7.7 meneruskan header W3C Trace Context (`traceparent`), trace dari client → Traefik → Zitadel core → Actions V2 webhook dapat divisualisasikan dalam satu Trace ID terpadu.

**`[TERKONFIRMASI]`** — Instrumentasi OTEL tidak terbatas pada main container Zitadel; komponen Login UI (Login V2) juga mendapat instrumentation OTEL, sehingga observabilitas mencakup seluruh stack autentikasi.

### 9.2 Konfigurasi Zitadel

Zitadel mendukung standard OTEL environment variables:

```env
OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
OTEL_SERVICE_NAME=zitadel
OTEL_TRACES_SAMPLER=always_on
```

**`[PERLU VERIFIKASI]`** — Nama envvar spesifik Zitadel seperti `ZITADEL_TRACING_*` perlu diverifikasi terhadap `defaults.yaml` instance Anda.

### 9.3 Contoh Konfigurasi OpenTelemetry Collector

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch: {}
  resourcedetection:
    detectors: [env, system]

exporters:
  prometheus:
    endpoint: 0.0.0.0:8889
  otlp/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true
  logging:
    verbosity: basic

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch, resourcedetection]
      exporters: [otlp/tempo, logging]
    metrics:
      receivers: [otlp]
      processors: [batch, resourcedetection]
      exporters: [prometheus, logging]
```

### 9.4 Traefik & OTEL

Traefik v3.7.7 mendukung native OpenTelemetry:

```yaml
tracing:
  otlp:
    grpc:
      endpoint: "otel-collector:4317"
      insecure: true

metrics:
  otlp:
    grpc:
      endpoint: "otel-collector:4317"
      insecure: true
```

---

## 10. Contoh Kode Tambahan

### 10.1 Login Flow (Authorization Code + PKCE) `[TERKONFIRMASI]`

```go
// 1. Generate PKCE verifier & challenge
codeVerifier := generateRandomString(64)
codeChallenge := base64URLEncode(sha256(codeVerifier))
state := generateRandomString(32)
nonce := generateRandomString(32)

saveToSession(session, "pkce_verifier", codeVerifier)
saveToSession(session, "oauth_state", state)

authURL := fmt.Sprintf(
  "%s/oauth/v2/authorize?client_id=%s&redirect_uri=%s&response_type=code"+
  "&scope=%s&state=%s&code_challenge=%s&code_challenge_method=S256&nonce=%s",
  zitadelDomain, clientID, urlEncode(redirectURI),
  urlEncode("openid profile email offline_access"),
  state, codeChallenge, nonce,
)
redirect(authURL)

// 2. Callback handler
func CallbackHandler(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    if q.Get("state") != getFromSession(r, "oauth_state") {
        http.Error(w, "invalid state", http.StatusBadRequest)
        return
    }
    code := q.Get("code")
    verifier := getFromSession(r, "pkce_verifier")

    tokenResp := postForm(zitadelDomain+"/oauth/v2/token", url.Values{
        "grant_type":    {"authorization_code"},
        "code":          {code},
        "redirect_uri":  {redirectURI},
        "client_id":     {clientID},
        "code_verifier": {verifier},
    })

    saveTokensToSession(r, tokenResp)
    redirect("/dashboard")
}
```

### 10.2 Validasi JWT `[TERKONFIRMASI]`

```go
package jwtvalidate

import (
	"context"
	"fmt"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

func ValidateZitadelToken(ctx context.Context, tokenString, jwksURL, issuer, audience string) (jwt.MapClaims, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("gagal load JWKS: %w", err)
	}

	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, k.Keyfunc,
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithValidMethods([]string{"RS256"}),
	)
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("token tidak valid: %w", err)
	}

	return claims, nil
}
```

### 10.3 Memanggil Management API (JWT Profile) `[TERKONFIRMASI]`

```go
func GetManagementAPIToken(ctx context.Context, zitadelDomain, keyFilePath, clientID string) (string, error) {
    assertion, err := buildSignedJWTAssertion(keyFilePath, zitadelDomain, clientID)
    if err != nil {
        return "", err
    }

    form := url.Values{
        "grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
        "assertion":  {assertion},
        "scope":      {"openid urn:zitadel:iam:org:project:id:zitadel:aud"},
    }

    resp, err := http.PostForm(zitadelDomain+"/oauth/v2/token", form)
    // parse access_token dari response
    return accessToken, err
}

func DeactivateUser(ctx context.Context, zitadelDomain, accessToken, orgID, userID string) error {
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
        fmt.Sprintf("%s/management/v1/users/%s/_deactivate", zitadelDomain, userID), nil)
    req.Header.Set("Authorization", "Bearer "+accessToken)
    req.Header.Set("x-zitadel-orgid", orgID)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("deactivate gagal, status: %d", resp.StatusCode)
    }
    return nil
}
```

---

## 11. Daftar Error Code Umum `[TERKONFIRMASI]`

| Kode / Error | Konteks | Arti |
|---|---|---|
| `unsupported_grant_type` ("password not supported") | Token endpoint | ROPC memang tidak ada di Zitadel v4 — bukan misconfig, jangan dicoba di-enable. Lihat §5.9.1 untuk headless flow. |
| `invalid_client` "empty client secret" | Token endpoint | Client confidential tidak mengirim secret, ATAU app tipe Web dipakai PKCE tanpa secret (Web SELALU confidential — §5.9.2). |
| `Errors.Target.DeniedURL` (COMMAND-NcJUKo) | POST /v2/actions/targets | Endpoint resolve ke IP yang diblokir HTTPClient.DenyList (default: semua CIDR private). Solusi dev: override `ZITADEL_HTTPCLIENT_DENYLIST` (§5.9.3). |
| "membership not found (AUTHZ)" | /v2/oidc/auth_requests | Bearer yang dipakai bukan login-client PAT (mis. token user biasa / sessionToken). Otentikasi endpoint ini WAJIB PAT login-client (§5.9.1). |
| "User could not be found (QUERY)" | /v2/sessions, /v2/users/me | loginName salah, ATAU PAT milik machine user login-client yang memang tidak punya user row (sifatnya, bukan error). loginName admin: `zitadel-admin@zitadel.<externaldomain>`. |
| "User already inactive (COMMAND)" | _deactivate | Idempotency: user sudah deactivated — reactivate dulu sebelum deactivate ulang (relevan utk script rerun). |
| `invalid_grant` | Token endpoint | Authorization code sudah dipakai/kadaluarsa |
| `invalid_client` | Token endpoint | `client_id`/`client_secret` salah |
| `invalid_request` | Authorize/Token | Parameter wajib hilang |
| `unauthorized_client` | Authorize/Token | Client tidak diizinkan memakai grant type tsb |
| `access_denied` | Authorize | User menolak consent, atau login gagal |
| `invalid_scope` | Authorize/Token | Scope tidak valid |
| `unsupported_grant_type` | Token | `grant_type` tidak dikenali |
| `login_required` | Authorize (prompt=none) | User belum login |
| `interaction_required` | Authorize | Perlu interaksi user tambahan |
| HTTP `401 Unauthorized` | REST API | Token hilang/invalid |
| HTTP `403 Permission Denied` | Management API | Token valid tapi tidak punya permission |
| HTTP `404 Not Found` | REST API | Resource tidak ditemukan |
| HTTP `409 Already Exists` | Create | Unique constraint violation |
| HTTP `429 Too Many Requests` | Semua endpoint | Rate limit terlampaui |

---

## 12. Ringkasan Perubahan vs Versi Sebelumnya

### 12.1 Poin-poin Kritis yang Sudah Diverifikasi `[TERKONFIRMASI]`

| Area | Detail | Status |
|---|---|---|
| Header signature | Dual header: `ZITADEL-Signature` dan `X-ZITADEL-Signature` | ✅ TERKONFIRMASI |
| Algoritma signature | HMAC-SHA256 dari `timestamp + "." + rawBody` | ✅ TERKONFIRMASI |
| Field response custom claims | `append_claims` (snake_case), **bukan** `appendClaims` | ✅ TERKONFIRMASI |
| Mode Target | 3 tipe: `restWebhook`, `restCall`, `restAsync` | ✅ TERKONFIRMASI |
| Format `targets` di Execution | Array of string ID langsung `["targetID"]` | ✅ TERKONFIRMASI |
| Function names | **Lowercase**: `preaccesstoken`, `preuserinfo`, `presamlresponse` | ✅ TERKONFIRMASI |
| Granulasi Event | 3 level: Event, Group, All | ✅ TERKONFIRMASI |
| Traefik scheme | **Wajib h2c** untuk gRPC-Web | ✅ TERKONFIRMASI |
| OTEL Login UI | Login V2 juga mendapat instrumentation OTEL | ✅ TERKONFIRMASI |

### 12.2 Perubahan Kritis dari Dokumen Sebelumnya

| Sebelumnya (v2.0) | Sekarang (v3.0) | Alasan |
|---|---|---|
| Function `PreTokenCreation` | `preaccesstoken` | Case-sensitive, lowercase wajib |
| Function `PreUserinfo` | `preuserinfo` | Case-sensitive, lowercase wajib |
| Header `ZITADEL-Signature` saja | Dual: `ZITADEL-Signature` dan `X-ZITADEL-Signature` | Ambiguitas dokumentasi resmi |
| 2 tipe Target | 3 tipe: + `restAsync` | Berdasarkan protobuf dan Terraform provider |
| Event kondisi hanya spesifik | + Group dan All | Fleksibilitas granulasi |

---

## 13. Checklist Sebelum Production

- [ ] Endpoint OIDC discovery document (`/.well-known/openid-configuration`) diverifikasi
- [ ] Actions V2 handler menggunakan **dual-header signature** (`ZITADEL-Signature` + `X-ZITADEL-Signature`)
- [ ] Function condition menggunakan **lowercase** (`preaccesstoken`, `preuserinfo`, dll)
- [ ] Custom claims handler menggunakan **`append_claims`** (snake_case), bukan `appendClaims`
- [ ] Target dibuat dengan **`restWebhook`**, **`restCall`**, atau **`restAsync`** (objek bertingkat)
- [ ] Execution `targets` sebagai array of string ID, bukan array object
- [ ] Traefik menggunakan **skema h2c** untuk komunikasi internal
- [ ] End-to-end test: signature verification berhasil, custom claims muncul di token
- [ ] Response time handler < timeout Target (terutama untuk `restCall`)
- [ ] PostgreSQL backup tersedia sebelum upgrade versi Zitadel

---
