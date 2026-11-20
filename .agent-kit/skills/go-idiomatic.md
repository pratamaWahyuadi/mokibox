---
name: hermes-go-idiomatic
description: >-
  Menegakkan Go idiomatik dan testable setiap kali menulis atau mereview kode
  Go, termasuk saat memakai net/http, Gin, atau Echo. Wajib dipakai untuk task
  apapun yang melibatkan penulisan handler, service, repository, atau package
  Go baru; menambah endpoint API di Gin/Echo; refactor kode Go yang error
  handling-nya berantakan; atau permintaan seperti "buatkan service X",
  "tambah endpoint Y", "bikin repository untuk Z". Pakai skill ini bahkan
  kalau user tidak secara eksplisit minta "kode idiomatik" — cukup mendeteksi
  konteks Go/Golang, Gin, atau Echo di request sudah cukup untuk trigger.
  Menegakkan prinsip accept interfaces return structs, error asli tidak
  pernah ditelan atau di-override, dan constructor pakai dependency injection
  supaya gampang di-mock dan ditest.
---

# Hermes Go Idiomatic

## Kenapa skill ini ada

Gin itu enak dipakai tapi punya kebiasaan buruk yang gampang menular ke kode: `c.Error(err)` dan `c.JSON(500, gin.H{"error": "internal error"})` sering dipakai untuk **membungkus lalu membuang** error asli. Yang sampai ke caller (dan ke log, dan ke test) bukan error Go yang sebenarnya terjadi, tapi string custom yang sudah di-generic-kan. Begitu error asli hilang di lapisan HTTP, seluruh chain di baliknya — service, repository, package lain — ikut kebiasaan yang sama karena "toh nanti juga di-override di handler". Ini yang mau dicegah skill ini.

Ingat juga: Gin dan Echo cuma wrapper tipis di atas `net/http`. `gin.Context` dan `echo.Context` pada akhirnya bungkus `http.ResponseWriter` dan `*http.Request`. Jadi prinsip Go idiomatik di bawah ini **tidak berubah** dan tidak boleh dikorbankan hanya karena pakai salah satu framework itu — perbedaannya cuma di lapisan paling luar (bagaimana request di-routing dan response di-serialize), bukan di cara struct, interface, atau error dirancang.

## Prinsip inti (urutan prioritas saat menulis kode baru)

1. **Accept interfaces, return structs.** Fungsi/constructor menerima parameter bertipe interface (didefinisikan di sisi consumer, sekecil mungkin — idealnya 1-3 method), tapi mengembalikan concrete struct. Ini yang bikin kode gampang di-mock tanpa perlu mock library berat.
2. **Error asli tidak pernah hilang.** Setiap error yang mungkin gagal, di-`return`, bukan di-log-lalu-ditelan (`if err != nil { log.Println(err); return nil }` adalah bug, bukan handling). Wrap dengan `fmt.Errorf("konteks: %w", err)` supaya `errors.Is`/`errors.As` masih bisa jalan sampai ke caller paling luar.
3. **Testable sejak baris pertama.** Kalau baru bisa kepikiran "nanti ditest belakangan", susunan kodenya sudah salah. Constructor yang menerima interface + tidak ada global state/singleton = otomatis testable tanpa perlu restrukturisasi besar nanti.
4. **Best practice standar**: package kecil dan fokus (bukan `utils` sampah-semua), nama package bukan `common`/`base`/`helper` generik, zero value yang berguna, tidak ada `panic` untuk error yang bisa di-handle, context.Context sebagai parameter pertama untuk operasi I/O.

Kalau ada tension antara "lebih cepat ditulis" vs prinsip di atas, prinsip di atas menang — kecuali user secara eksplisit minta prototype/throwaway script.

## Pola: accept interface, return struct

Definisikan interface di package yang **memakainya**, bukan di package yang mengimplementasikannya. Interface kecil.

```go
// package service — interface didefinisikan di sini karena service yang butuh
type UserRepository interface {
    FindByID(ctx context.Context, id string) (*User, error)
    Save(ctx context.Context, u *User) error
}

type UserService struct {
    repo   UserRepository // dependency di-inject lewat interface
    logger *slog.Logger
}

// constructor menerima interface, mengembalikan concrete struct (bukan interface)
func NewUserService(repo UserRepository, logger *slog.Logger) *UserService {
    return &UserService{repo: repo, logger: logger}
}

func (s *UserService) GetUser(ctx context.Context, id string) (*User, error) {
    u, err := s.repo.FindByID(ctx, id)
    if err != nil {
        return nil, fmt.Errorf("get user %s: %w", id, err)
    }
    return u, nil
}
```

Kenapa constructor return struct, bukan interface: caller yang butuh interface tinggal definisikan sendiri interface kecil sesuai kebutuhannya (Go idiom: "return concrete, accept interface"). Return interface dari constructor cuma masuk akal kalau memang sengaja menyembunyikan implementasi (mis. untuk plugin/strategy pattern) — bukan default.

Saat menulis test, mock `UserRepository` tinggal implement 2 method itu — tidak perlu mock framework kalau interface-nya kecil:

```go
type mockUserRepo struct {
    findByIDFn func(ctx context.Context, id string) (*User, error)
}

func (m *mockUserRepo) FindByID(ctx context.Context, id string) (*User, error) {
    return m.findByIDFn(ctx, id)
}
func (m *mockUserRepo) Save(ctx context.Context, u *User) error { return nil }
```

### Trap: DI diterapkan di satu layer saja (middleware vs handler)

Pola accept-interface sering benar di middleware/auth (verifier +
store di-inject sebagai interface kecil, stub-able) tapi berhenti
sebelum sampai layer handler — handler menerima concrete
`*Queries` / `*sql.DB` / client eksternal langsung. Konsekuensi
yang diam-diam mahal:

- Layer yang pakai interface, teruji; layer yang concrete
  membutuhkan infra asli untuk ditest, jadi akhirnya TIDAK
  DITEST sama sekali.
- Business logic justru menumpuk di layer yang tak teruji itu —
  handler menjadi file terbesar di codebase karena "toh gampang
  akses Queries di sini". Magnet logic + nol test = kombinasi
  terburuk.
- Review heuristic: cari package dengan business logic tebal
  TANPA SATU PUN file `_test.go`. Kalau ketemu, cek
  constructor-nya — hampir pasti pegang concrete I/O deps. Itu
  bukan "belum sempat nulis test"; itu struktur yang MENOLAK
  test.

Perbaikannya bukan men-test paksa dengan infra asli, tapi
menarik interface ke consumer: tiap handler mendefinisikan
interface kecil berisi method yang DIA panggil (bukan interface
raksasa satu-untuk-semua), plus interface begin-tx bila perlu.
Production wiring tetap pass concrete dari `main.go`; test pass
stub.

## Pola: error asli, bukan error dibungkam

Aturan yang wajib dicek untuk setiap error yang ditangani:

- **Jangan pernah** `if err != nil { return nil }` tanpa mengembalikan/mencatat error-nya.
- **Jangan pernah** menukar error asli dengan string generic sebelum sampai ke lapisan yang benar-benar butuh generic message (biasanya cuma di response HTTP paling luar).
- Wrap dengan konteks, bukan cuma diteruskan mentah — `%w` supaya chain-nya tetap bisa di-unwrap:

```go
if err != nil {
    return fmt.Errorf("fetch order %d from db: %w", orderID, err)
}
```

- Kalau butuh membedakan jenis error di layer atas, pakai sentinel error atau custom error type + `errors.Is` / `errors.As`, bukan string matching (`strings.Contains(err.Error(), "...")` adalah anti-pattern):

```go
var ErrOrderNotFound = errors.New("order not found")

// di repository
if rows == 0 {
    return fmt.Errorf("order %d: %w", id, ErrOrderNotFound)
}

// di handler / caller
if errors.Is(err, ErrOrderNotFound) {
    c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
    return
}
```

Baru di titik inilah wajar mengubah error jadi response message yang lebih generic — karena itu **batas terluar** (boundary layer), bukan di tengah-tengah business logic. Log error asli (dengan `%+v`/`slog` structured field) di titik ini juga, sebelum di-generic-kan ke response.

## Pola handler: net/http vs Gin vs Echo

Karena Gin dan Echo cuma wrapper `net/http`, service/repository di bawahnya **tidak boleh tahu** framework apa yang dipakai di atasnya — tidak boleh ada `*gin.Context` atau `echo.Context` bocor ke dalam business logic. Business logic cuma boleh bergantung pada `context.Context` standar.

**net/http (baseline, paling idiomatik):**
```go
func (h *OrderHandler) GetOrder(w http.ResponseWriter, r *http.Request) {
    order, err := h.svc.GetOrder(r.Context(), chi.URLParam(r, "id"))
    if err != nil {
        h.writeError(w, err) // satu tempat yang tahu cara map error -> status code
        return
    }
    json.NewEncoder(w).Encode(order)
}
```

**Echo (idiomatik karena handler memang return error — manfaatkan ini, jangan swallow di dalam handler):**
```go
func (h *OrderHandler) GetOrder(c echo.Context) error {
    order, err := h.svc.GetOrder(c.Request().Context(), c.Param("id"))
    if err != nil {
        return err // biarkan mengalir ke central HTTPErrorHandler Echo
    }
    return c.JSON(http.StatusOK, order)
}

// didaftarkan sekali di setup, bukan diulang di tiap handler
e.HTTPErrorHandler = func(err error, c echo.Context) {
    status, msg := mapErrorToHTTP(err) // satu titik mapping, error asli tetap di-log di sini
    slog.Error("request failed", "err", err, "path", c.Path())
    c.JSON(status, echo.Map{"error": msg})
}
```

**Gin (di sinilah godaan `c.Error` + `gin.H{"error": "..."}` biasa muncul — hindari, pakai pola ini sebagai gantinya):**
```go
func (h *OrderHandler) GetOrder(c *gin.Context) {
    order, err := h.svc.GetOrder(c.Request.Context(), c.Param("id"))
    if err != nil {
        h.respondError(c, err) // helper terpusat, bukan inline gin.H tiap handler
        return
    }
    c.JSON(http.StatusOK, order)
}

// satu helper terpusat, error asli di-log sebelum di-generic-kan
func (h *OrderHandler) respondError(c *gin.Context, err error) {
    slog.Error("request failed", "err", err, "path", c.FullPath())
    switch {
    case errors.Is(err, ErrOrderNotFound):
        c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
    case errors.Is(err, ErrInvalidInput):
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
    default:
        c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
    }
}
```

Poin pentingnya: mapping error → HTTP status **cuma boleh terjadi di satu tempat per handler-group** (helper/middleware terpusat), bukan berulang di tiap handler dengan detail berbeda-beda. Dan error asli (`err` yang lengkap, bukan cuma pesan generic) selalu di-log sebelum diubah jadi response — jangan sampai error yang benar-benar terjadi cuma bisa dilihat user sebagai "internal server error" tanpa jejak di log.

### Helper domain lintas handler: jangan method di struct satu handler

Logika domain yang dipakai LEBIH DARI SATU handler (mis.
visibility check "apakah viewer boleh lihat resource X") sering
lahir sebagai method di handler pertama yang membutuhkannya.
Saat handler kedua butuh logika sama, dia tidak bisa memanggil
method milik struct lain — jalan pintasnya: copy-paste inline.
Duplikasi inline aturan domain (visibility, quota, ownership)
adalah drift-bug factory: fix di satu salinan tidak sampai ke
salinan lain, dan kedua salinan mulai berbeda diam-diam (satu
tambah kondisi baru, satu tidak).

Aturan: helper yang dipakai lintas handler = free function
(atau struct helper sendiri) yang menerima interface kecil,
ditempatkan di level package — bukan method di salah satu
handler. Method private boleh untuk logika yang benar-benar
cuma milik handler itu.

### Helper parse: return error, bukan tulis response + bool

Dua pola helper parse di Echo/framework:

- **A (kurang idiomatik)**: helper menulis error response
  sendiri lalu return `bool`. Caller jadi `_ = respond(...)`
  (error discard) dan ada dua titik yang menulis response.
  Kalau respond helper gagal, error-nya tertelan.
- **B (idiomatik)**: helper return error yang sudah di-wrap
  sentinel; caller punya SATU titik respond:
  `if err != nil { return respond(c, err) }`.

Pola A tidak selalu salah — kalau konsisten di semua call site
dan respond helper praktis tidak pernah gagal, bisa diterima.
Tapi B default yang lebih aman: satu titik tulis response per
handler, error tidak pernah di-discard, dan helper bisa dipakai
ulang di context non-HTTP (background job, test) tanpa perlu
framework context.

## Pola: Resume/Checkpoint bug — state di disk tidak direload ke memori

Sistem pipeline berbasis checkpoint file (resume = "baca file mana yang ada,
lanjut dari situ") punya failure mode yang berulang: fungsi orkestrasi
declare variabel konteks kosong, load SEBAGIAN checkpoint saat resume, lalu
stage lanjutan memakai variabel yang tidak pernah diisi. Contoh nyata
(prdgen): resume ke validate_prd mengirim PRD kosong ke validator padahal
PRD.md utuh di disk; resume ke deep-dive memaksa user jawab gate ulang
padahal pertanyaan sudah tersimpan.

Aturan: SETIAP stage yang mengonsumsi artefak dari stage sebelumnya harus
guaranteed konteksnya terisi saat entry-point-nya bisa dilompati lewat
resume. Test-nya wajib mengeksekusi pipeline PENUH dengan kondisi "semua
checkpoint ada, mulai dari stage terakhir" — bukan cuma happy-path
run-lurus, karena run-lurus menyembunyikan bug ini (variabel keisi dari
memori, bukan dari disk).


- Jangan pake goroutine di handler HTTP tanpa context cancellation.
- Pake `errgroup.Group` kalo butuh paralelisme dengan error propagation.
- Jangan pake `time.Sleep` buat nunggu async task. Pake `WaitGroup` atau channel.
- Selalu pass `context.Context` ke goroutine, jangan biarin leak.

### Terminal-state writes JANGAN mewarisi ctx yang bisa expired

Saat pipeline utama dibungkus `context.WithTimeout(parent, budget)` (mis.
transcode job, request handler, batch job), dan kill path fire karena
budget habis — **semua write DB yang terjadi setelahnya** (cleanup
status, retry bumps, final-state UPDATE) akan mewarisi ctx yang
sudah deadline-exceeded. Hasilnya: UPDATE gagal secara diam-diam,
row stuck di state transient selamanya (invisible ke user, invisible
ke cleanup job).

Solusi: untuk terminal-state writes, pakai fresh ctx:
```go
func markFailedDetached(videoID uuid.UUID) error {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    _, err := w.Queries.MarkVideoFailed(ctx, videoID)
    return err
}
```
Kelas bug yang sama: handler `ProbeFile` yang me-return
`fmt.Errorf("%w: %s", err, stderr)` menelan error asli `cmd.Run()`
— log operator tidak bisa bedakan killed-probe vs corrupt-file.
Prinsip umum: **error/ctx asli harus survive sampai log**; kalau Anda
meng-overwrite pesan error untuk grouping, preserve via `%v` atau
`%w` agar info tidak hilang.

## Pola: Database (generic)

- Semua operasi DB nerima `context.Context` sebagai parameter pertama —
  tidak ada query tanpa context, apapun driver/ORM-nya.
- Jangan `SELECT *`. Explicit column list, apapun query builder-nya.
- No-rows itu bukan error aplikasi — cek pakai sentinel error dari driver
  yang dipakai (`errors.Is(err, sql.ErrNoRows)` atau setara), jangan
  treat sebagai nil/empty struct secara diam-diam.
- Kalau repo ini pakai lebih dari satu driver/pool DB sekaligus, atau
  pakai code generator query (sqlc, dsb) — itu keputusan project, cek
  CONVENTIONS.md untuk aturan spesifiknya, jangan asumsi dari skill ini.

## Pola: Testing

- Unit test service pake mock interface (bukan mock library).
- Jangan test implementation detail. Test behavior.
- Satu test = satu skenario. Jangan bikin test yang nge-test 5 hal sekaligus.
- Kalau repo ini punya pola integration/smoke test tersendiri, itu ada di
  CONVENTIONS.md — pola generic di sini cuma buat unit test.
- Table-driven test + `t.Parallel()` untuk pure function: paralel
  gratis dan bebas flake untuk fungsi tanpa shared state. Subtest
  yang menyentuh I/O atau mock stateful JANGAN diparalelkan.
- **Testable-core pattern** untuk orchestrator ber-I/O (transcode
  pipeline, batch job, multi-step service): ekstrak bagian pure
  (arg building, parsing, mapping, formatting) ke fungsi
  terpisah + table test untuk itu; orchestrator tetap tipis
  (urutan langkah + error handling + terminal-state writes) dan
  diverifikasi integration/smoke — bukan unit test
  mock-everything yang mengetest wiring, bukan behavior.
- Deteksi structural smell saat review: package dengan business
  logic paling tebal tapi nol file test — root cause-nya hampir
  selalu concrete I/O dependency (lihat trap DI lintas layer di
  atas), bukan "belum sempat nulis test". Perbaiki strukturnya
  dulu; jangan paksa test dengan infra asli.

## Project-Specific Overrides

Sebelum menerapkan pola generic di atas, cek apakah repo ini punya
`CONVENTIONS.md` di root. Kalau ada, itu override/tambahan yang lebih
spesifik dari skill ini (nama helper, sentinel error, dual-pool/dual-driver
rule, pattern test tertentu, dsb) — ikuti itu untuk hal yang bertentangan.
Skill ini isinya Go generic dan berlaku lintas project; jangan duplikat
isi CONVENTIONS.md ke sini, dan jangan sebut nama project apapun di sini.
## Checklist self-review sebelum kode dianggap selesai

Sebelum menyerahkan kode Go, jalankan cek ini terhadap kode yang baru ditulis:

- [ ] Apakah ada `err != nil { return nil }` atau sejenisnya tanpa propagate/log error?
- [ ] Apakah ada error yang di-string-match (`strings.Contains(err.Error(), ...)`) padahal bisa pakai sentinel error + `errors.Is`?
- [ ] Apakah constructor menerima interface (bukan concrete struct) untuk setiap dependency yang punya I/O (DB, HTTP client, dsb)?
- [ ] Apakah constructor mengembalikan struct konkret (bukan interface), kecuali memang sengaja menyembunyikan implementasi?
- [ ] Apakah interface didefinisikan sekecil mungkin, di sisi consumer?
- [ ] Apakah `*gin.Context`/`echo.Context`/`*http.Request` bocor ke dalam service/repository layer?
- [ ] Apakah mapping error→HTTP status terpusat di satu tempat, bukan diulang-ulang?
- [ ] Kalau ini kode baru: apakah strukturnya sudah memungkinkan ditulis unit test tanpa spin up DB/network asli (lewat mock interface)?
- [ ] Semua layer yang menyentuh I/O (middleware DAN handler/service)
      menerima interface consumer-side — bukan cuma middleware?
      (Trap: handler yang pegang concrete = package terbesar + nol
      test.)
- [ ] Helper domain yang dipakai >1 handler = free function di
      level package, bukan method di struct salah satu handler?
      (Cek: adakah aturan domain yang ter-copy inline di handler
      kedua?)
- [ ] Package dengan business logic terbesar punya unit test?
      Kalau tidak, dependency-nya concrete — perbaiki struktur
      dulu, jangan paksa test dengan infra asli.

Kalau ada satu poin gagal, perbaiki dulu sebelum menganggap task selesai — jangan hanya menyebutkan "catatan: bisa diperbaiki nanti".

