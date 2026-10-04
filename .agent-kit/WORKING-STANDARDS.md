# Working Standards (Agent Contract)

Bagian ini adalah **kontrak kerja** untuk AI agent maupun programmer manusia yang
bekerja di MokiBox. Ini adalahstandarisasi lintas contributor — bukan dokumentasi
historis. Semua poin di sini wajib dipatuhi; kalau ada yang tidak bisa dipenuhi,
sebutkan eksplisit di PR/HANDOFF, jangan diam-diam dilewati.

Sumberseeded dari MEMORY.md (personal notes owner project) yang di-generalisasi
supaya berlaku untuk semua agent, bukan cuma satu agent.

---

## Role

Agent **implement**, manusia **architect**. Agent mengeksekusi, meng-flag concern
dengan opsi konkret, dan **tidak pernah memutuskan trade-off sendiri**.

Kalau ada keputusan yang butuh judgment (arsitektur, prioritas, breaking change),
agent siglasih dengan 2+ opsi konkret + bukti, lalu menunggu keputusan manusia.

## Source of Truth Priority

1. **Planning docs** (`planning/PRD.md`, `LLD_PLAN.md`, `03_schema.md`,
   `04_api_contracts.md`, `ROADMAP_TIKTOK_PARITY.md`) — arsitektur, spec, schema,
   contract
2. **Skills** (`.agent-kit/skills/`) — prosedur kerja umum
3. **Repo files** — `CONVENTIONS.md` (gotcha berulang), `HANDOFF.md` (state/lesson
   per fase)
4. **Working Standards ini** — gaya kerja global, **tidak pernah** untuk detail
   spesifik proyek

Kalau dua sumber bertentangan, yang lebih atas menang — dan agent wajib
mention konfliknya, bukan diam-diam pilih satu.

## Session Start Checklist (WAJIB, berurutan)

- [ ] Baca planning docs yang relevan dengan task saat ini. Kalau ada yang
      **missing atau stale**, flag segera ke manusia — **jangan proceed dengan
      asumsi**.
- [ ] Baca `CONVENTIONS.md` dan `HANDOFF.md`.
- [ ] Jalankan `git log -1`, `gh pr list`, `gh issue list --state open` —
      **jangan percaya snapshot yang gelap**.
- [ ] Load skills yang relevan dari `.agent-kit/skills/` (lihat `README.md` untuk
      index). Kalau ada gap (mis. tidak ada skill untuk area yang sedang dikerjakan),
      sebutkan di pesan pertama ke manusia.

**Jangan tulis kode sebelum checklist ini selesai.**

## Merge adalah Keputusan Manusia

- Agent **tidak pernah self-merge** PR.
- Setelah PR siap, tanyakan: merge sekarang atau nanti.
- Perubahan deploy-infra (VPS, aaPanel, nginx, cert) hidup di branch khusus
  (`deploy/aapanel`), bukan `main`.

## Deviasi dari Spec: di Patch Inline, Bukan Dicatat

Generalisasi dari PR #37 fase 4:

- Kalau LLD/plan mendeskripsikan behavior yang **tidak ada** di acceptance criteria
  issue → **fix gap-nya inline di commit yang sama**, jangan dibawa sebagai
  "DEVIATION: not implemented" di PR body.
- Kata "deviasi" hanya dipakai untuk **blocker** yang butuh diskusi desain atau
  dependensi eksternal.
- Kalau memang tidak bisa dipenuhi, tulis di PR body sebagai:
  ```
  - [ ] ~~criterion yang tidak terpenuhi~~
        — **DEVIATION**: alasan teknis + workaround
  ```
  **Jangan menghapus atau menyembunyikan acceptance line.**

## Bug Ditemukan Saat Eksekusi: STOP dan Bukti Dulu

Pola ini terbukti menemukan banyak bug tersembunyi di MokiBox:

1. **STOP kode, kumpulkan bukti** — log error lengkap, payload capture (untuk bug
   kontrak eksternal: listener debug di network dependency, capture body asli),
   query DB, listing object storage. Contoh di doc **bukan bukti kontrak**.
2. **Laporkan ke manusia via clarify** dengan format persis:
   - langkah reproduksi
   - expected vs actual
   - root cause (dengan bukti)
   - 2-3 opsi (patch inline vs tunda ke issue terpisah) + "opsi Anda sendiri"
3. **Patch hanya setelah manusia memilih** — lalu catat decision trail di commit
   body (commit `fix:` terpisah per bug).
4. Jangan gabungkan fix ke dalam commit fitur.

## External Integration Rule

Saat integrasi dengan dependency eksternal (OIDC provider, webhook sender, SDK):

- **Capture payload/perilaku live sebelum menulis handler** yang membaca field-nya.
  Contract berbeda antar versi dan antara event self-acted vs admin-triggered.
- **Smoke harus meng-exercise endpoint asli (HTTP)**, bukan side effect-nya
  (file/DB). Helper smoke bisa PASS sementara endpoint-nya broken.

## Test Script Rerun-Ability

Integration script dengan langkah yang **mutate state** (deactivate user, delete
resource, insert row) **wajib reset state itu di awal script** — jangan asumsikan
environment fresh.

Buktikan dengan **menjalankan script 2x back-to-back sebelum commit**. Rerun yang
gagal karena sisa state run sebelumnya **membatalkan klaim** "siapa pun bisa rerun
ini".

## Standard Git

- Branch: `feature/<fase>` untuk roadmap fase (`git-phase-workflow`),
  `improve/<id>` untuk issue improvement (`git-improvement-workflow`).
- Commit: `feat: [<issue-id>] <deskripsi>`. Id issue konsisten di branch, commit,
  dan PR.
- **Atomic commit per bagian logis** — jangan gabung issue berbeda dalam 1 commit.
- Body commit panjang/backtick → tulis ke file, `git commit -F <file>`
  (Hermes parser block inline payload besar).
- **Self-check angka sebelum tulis di PR body / commit message** — grep ulang
  setiap angka (route count, query count, file count). Issue text sering punya
  angka stale; angka yang salah di self-report lebih buruk dari tidak ada angka.
- **Tidak ada `git reset --hard`** di session interactive (Hermes BLOCK).
  Untuk sinkron main: `git checkout main && git pull --ff-only origin main`.
- **Prefer additive commits over history rewrite** — `--amend` selalu modify HEAD;
  edit commit lain pakai `git commit --fixup=<sha>` + `rebase -i`, dan konfirmasi
  ke manusia sebelum force-push.
- Tidak ada commit langsung ke `main`. Tidak ada PR chaining (PR base selalu `main`).

## Kapan Update Repo Files

- **`CONVENTIONS.md`** — gotcha berulang yang belum tercakup skill (bukan
  one-off). Usulkan penambahan ke manusia.
- **`HANDOFF.md`** — akhir setiap fase/sesi: done, WIP, deviasi, cek next-phase.

## Test Coverage

- Semua handler wajib punya unit test dengan **mock store** (consumer-side
  interface, bukan mock package internal) — lihat skill `mokibox-go-shared`.
- Test yang Sentuh I/O wajib punya test di mana I/O-nya di-stub.
- Integration test (HTTP nyata) hanya untuk flow yang tidak bisa dicakup unit.

## Anti-Patterns

- Pakai working standards ini sebagai catch-all — ketidaknyaman itu sinyal
  isinya harus pindah ke repo file (CONVENTIONS/HANDOFF) atau skill.
- Duplikasi isi skill di sini.
- Menebak ketika tidak yakin — tanya, per aturan di atas.
- Claim "smoke PASS" padahal yang dites cuma `build`/`vet`/`test`. Sebut static
  check apa yang PASS vs runtime smoke apa yang PASS.
- Menjalankan Smoke lewat helper/DB direct lalu mengklaim endpoint-nya bekerja.