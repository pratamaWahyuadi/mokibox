---
name: git-improvement-workflow
description: "Improvement issues: one branch per issue, not per phase."
version: 1.0.0
author: pratama
license: MIT
metadata:
  hermes:
    tags:
      - git
      - workflow
      - improvement
      - issue-driven
    related_skills:
      - git-phase-workflow
      - agent-ready-issues
      - tdd
---

# Git Improvement Workflow

## When to Use

Use this skill when the user asks to work on issues from an improvement/backlog
file or roadmap:
- "kerjakan issue improvement", "jalankan P0-x dari roadmap"
- mentions `issue_improvement.json`, `ISSUE_IMPROVEMENT.json`, or any improvement
  JSON whose issues have inter-issue dependencies (`blocked by`, `harus selesai
  sebelum`, `depends-on`)
- issues whose `phase` field is a priority tier (`P0`, `P1`, `P2`) rather than a
  sequential development phase, and which carry an `enhancement`/`feature` label

Do NOT use this skill for prdgen-generated phase roadmaps (`phase: "Fase N"`,
sequential issues inside a phase) — that is `git-phase-workflow`.

Use when user asks "kerjakan issue improvement", "jalankan P0-x dari roadmap", atau
menyebut file improvement JSON (issue_improvement.json / ISSUE_IMPROVEMENT.json)
yang punya depends-on antar-issue. BUKAN untuk roadmap fase prdgen — itu
`git-phase-workflow`.

Skill ini mengatur eksekusi issue dari improvement roadmap: kumpulan issue yang
menjelaskan gap/parity sudah ditulis lengkap (signature API, SQL, acceptance
criteria, out of scope) tapi bukan roadmap fase berurutan hasil prdgen.

## Kapan pakai skill ini vs git-phase-workflow

| Aspek | git-phase-workflow | skill ini |
|---|---|---|
| Unit branch | 1 fase = 1 branch = banyak issue | **1 issue = 1 branch = 1 PR** |
| Issue id | `phase-N.M` (titik) | bebas — biasanya `P0-1`, `IMP-3`, `GH #57` |
| Prefix commit | `feat: [phase-N.M] ...` | `feat: [P0-1] ...` (id dari issue) |
| Urutan | roadmap fase berurutan | **topologi dependensi** — unblocked dulu, sisanya boleh paralel |
| Merge | semua issue selesai lalu 1 PR | **satu PR per issue, merge per issue** |
| Branch | `feature/phase-N` | `improve/<id-slug>` |
| Peran | urutan linear | partly-parallel + explicit `blocked by` |

Aturan pemilihan:
- JSON berisi roadmap fase prdgen (`phase: "Fase N"`) → `git-phase-workflow`
- JSON berisi improvement/parity/backlog (`phase: "P0"`/`"P1"`, label
  `enhancement`/`feature`, ada pointer `roadmap:` terpisah) → **skill ini**

## Prasyarat sebelum mulai

1. Repo git valid: `git rev-parse --is-inside-work-tree`
2. Working tree bersih: `git status --porcelain`. Kalau kotor, tanya user, jangan
   timpa perubahan uncommitted.
3. **Sinkronkan main dengan benar** (bukan cuma "up to date" karena fetch stale):
   ```bash
   git fetch origin main
   git log --oneline origin/main -3
   git log --oneline main -3
   ```
   Kalau `origin/main` lebih baru → `git checkout main && git pull --ff-only origin main`.
   **Jangan `git reset --hard`** — Hermes BLOCK di session interactive. Kalau
   benar-benar perlu reset, konfirmasi dulu ke user apakah ada WIP uncommitted.
4. **Baca file improvement JSON** dan petakan topologi dependensi SEBELUM eksekusi:
   ```bash
   python3 - <<'PY'
   import json, re
   items = json.load(open("path/issue_improvement.json"))
   if isinstance(items, dict): items = items.get("issues", items)
   for it in items:
       body = it.get("body", "")
       dep = re.findall(r"blocked by #(\d+)|harus selesai sebelum (P\d+-\d+)", body, re.I)
       print(it.get("title","")[:50], "|", it.get("labels"), "| blocked_by:", dep)
   PY
   ```
   Hasilnya = graf dependensi. Lanjut ke bagian Topologi Dependensi.

## Alur Eksekusi

### Tahap 1 — Petakan topologi, tampilkan checklist

Baca SEMUA issue di JSON dulu (jangan eksekusi sambil baca, urutan penting).
Kelompokkan jadi Tier 0 (unblocked) dan Tier 1+ (yang depends on tier sebelumnya).

Tampilkan ke user dalam 1 pesan: daftar per tier, mana yang paling isolated (bisa
paralel), mana yang paling risky. Minta konfirmasi urutan atau apakah mau kerjakan
semuanya/sebagian dulu. Jangan mulai coding sebelum plan user-visible.

### Tahap 2 — Satu issue = satu branch

```bash
git checkout main && git pull --ff-only origin main
git checkout -b improve/{id-slug}
```

`id-slug` = id issue lowercase + slug judul pendek:
- `P0-1` → `improve/p0-1-search`
- `P0-2` → `improve/p0-2-migration-hashtags`
- `P0-10` → `improve/p0-10-block-mute`
- issue GH `#57` tanpa id internal → `improve/gh-57-search`

Satu issue satu branch. Kalau issue punya beberapa bagian besar dan secara teknis
independen (misal schema vs handler), BISA dipecah jadi beberapa branch kecil,
tapi default-nya satu branch per issue.

### Tahap 3 — Kerjakan issue, atomic commit per bagian logis

Baca body issue sepenuhnya dulu (lihat struktur di bawah). Ikuti SEMUA acceptance
criteria dan out-of-scope.

Untuk setiap bagian logis (bukan satu commit untuk semuanya):
1. Kerjakan bagian itu saja.
2. Verifikasi bagian itu (build/test yang relevan).
3. Commit: `feat: [{issue-id}] {deskripsi bagian}`.

Contoh issue dengan beberapa bagian independen:
```bash
git add migrations/003_x.sql sqlc/sqlc.yaml
git commit -F /tmp/commit-msg-part1.txt
# feat: [P0-2] migration 003_hashtags — schema + grants + sqlc config

git add shared/db/hashtags.sql.go api-gateway/handlers/hashtag.go
git commit -F /tmp/commit-msg-part2.txt
# feat: [P0-2] handler + query untuk halaman hashtag
```

Commit body wajib berisi:
- Root cause / motivation singkat (1-2 kalimat)
- File yang disentuh + breakdown angka (wajib konsisten, self-check dulu)
- Verification commands + expected output

Tulis body ke file dulu (Hermes parser block commit inline yang panjang), lalu
`git commit -F <file>`.

### Tahap 4 — Verifikasi issue-level sebelum PR

**WAJIB: self-check angka sebelum tulis PR body.** Setiap angka total di body PR
WAJIB di-grep ulang saat itu juga, bukan taken from issue body atau memory:
```bash
grep -c "^\s*api\." api-gateway/routes.go   # route count
grep -c '^-- name:' sqlc/queries/*.sql      # query count
git diff --stat main..HEAD | tail -1        # file count
```
Issue text sering punya angka stale. Selalu grep ulang sebagai ground truth.

Smoke test wajib sesuai tabel Runtime smoke di bawah.

### Tahap 5 — PR per issue, merge per issue

```bash
git push -u origin improve/{id-slug}
gh pr create --base main --head improve/{id-slug} \
  --title "feat: [{issue-id}] {judul ringkas}" \
  --body-file /tmp/pr-body.md
```

**Jangan self-merge tanpa clarify.** Setelah PR siap, tanya user: merge sekarang
atau later? Kalau user bilang merge, baru `gh pr merge`.

Setelah merge:
```bash
git checkout main && git pull --ff-only origin main
git branch -d improve/{id-slug}
```

Lalu pindah ke issue berikutnya (Tier 0 berikutnya atau issue yang unblocked setelah
merge ini).

## Struktur Body Improvement Issue (yang harus dibaca)

| Section | Isi | Yang harus dilakukan |
|---|---|---|
| `## Konteks` | Kenapa gap ini penting | Baca dulu, tells you why it matters dan sering explain priority |
| Section dengan ikon peringatan | Peringatan / gotcha | **PALING PENTING, baca duluan** — sering berisi hal yang paling sering terlupakan |
| `## Signature API` / `## API` | Endpoint contract | Ikuti persis, jangan improvisasi format response |
| `## Query` / `## Migration` / `## Schema` | SQL / codegen | Ikuti persis termasuk nama query |
| `## File yang disentuh` | Daftar file | Cross-check dengan `git status` setelah selesai |
| `## Konvensi yang harus dipatuhi` | Aturan repo | Baca skill domain-specific project untuk rule tambahan |
| `## Acceptance criteria` | Checklist `- [ ]` | Tick semua yang bisa di-check, tulis evidence di PR body |
| `## Definition of done` | Smoke + contract doc + HANDOFF | Ikuti SEMUA, ini bagian yang sering di-skip |
| `## Out of scope` | Yang TIDAK boleh dikerjakan | **HARD BOUNDARY**, jangan dikerjakan meski terlihat mudah |

## Topologi Dependensi dan Jadwal

Cara baca dependensi dari body issue: cari pattern `Blocked by #N`,
`harus selesai sebelum P0-X`, `setelah ... sudah ada`, `Depends on: #N`. Buat tabel
manual: issue | blocked by | unblocked.

Contoh jadwal:
```text
Tier 0 (unblocked): P0-1, P0-2, P0-5, P0-6, P0-8, P0-9, P0-10
Tier 1 (blocked):   P0-3 (<- P0-2), P0-4 (<- P0-1), P0-7 (<- P0-6)
```

Tier 0 dikerjakan satu-per-satu (satu branch/PR masing-masing). Kalau user mau
mengerjakan yang independent secara paralel, pakai branch terpisah per issue lalu
merge berurutan. Tier 1 hanya bisa mulai setelah issue pemblokirnya merged ke main.

Internal blocked by (dalam satu issue): misal P0-2 punya bagian schema dan bagian
query. Itu bukan dua issue, tapi bisa dipecah jadi dua commit di satu branch
(Tahap 3).

## Runtime Smoke (tabel generik backend project)

| Jenis perubahan | Static check | Runtime smoke |
|---|---|---|
| Migration + query baru | `build` + `vet` | **Ya** — DB roundtrip, verify query return data benar |
| Handler / route baru | `build` + `vet` | **Ya** — HTTP smoke via binary + curl, bukan DB direct |
| Refactor tanpa API change | `build` + `vet` | Tidak wajib |
| Docs / planning saja | tidak perlu | tidak |

Smoke harus lewat endpoint asli, bukan helper. Smoke lewat DB direct atau helper
function bisa PASS sementara endpoint-nya broken (pola the 5-bug lesson: helper
smoke PASS sementara endpoint broken).

## Out-of-scope handling

Issue improvement sering menyebut Out of scope yang berguna untuk reviewer juga
(Known Limitation). Kalau kamu encounter ini:
- **Jangan dikerjakan** (hard boundary).
- Tulis di PR body sebagai Known Limitation yang eksplisit.
- Kalau yakin itu gap yang berarti dan user akan menganggapnya penting, buat
  issue baru via `gh issue create` DAN sebutkan di PR body ("sudah saya buat issue
  terpisah: #XX").

**Jangan senyapkan DEVIATION.** Kalau acceptance criteria tidak bisa dipenuhi
penuhnya, tulis di PR body sebagai:
```
- [ ] ~~criterion yang tidak terpenuhi~~
      — **DEVIATION**: alasan teknis + workaround
```
bukan menghapus atau menyembunyikan.

## Skill pendukung (project-specific)

Skill ini **universal, tidak terikat project**. Untuk workflow project spesifik,
baca skill domain-nya (misal `mokibox-go-shared` untuk konvensi Go + deployment
topology, `tdd` untuk unit test pattern).

## Constraints

1. **Tidak ada commit langsung ke main** — semua lewat branch + PR.
2. **Tidak ada PR chaining** — semua PR base `main`.
3. **Atomic commit per bagian logis** — jangan commit semua issue dalam 1 commit.
4. **Format commit wajib** `feat: [{issue-id}] {deskripsi}` — `{issue-id}` dipakai
   konsisten di branch, commit, PR.
5. **Self-check angka sebelum tulis di PR body / commit message** — grep ulang
   setiap angka, jangan taken from issue body.
6. **Jangan `git reset --hard`** di session interactive (Hermes BLOCK).
7. **Prefer additive commits over history rewrite** — `--amend` selalu modify HEAD;
   untuk edit commit lain pakai `git commit --fixup=<sha>` + `rebase -i`.
8. **Post-merge: hapus branch** — `git checkout main && git branch -d improve/{id-slug}`.
9. **Setelah semua issue selesai** — update planning doc atau roadmap status kalau ada.

## Referensi cepat perintah

| Aksi | Command |
|---|---|
| Sinkron main | `git checkout main && git pull --ff-only origin main` |
| Buat branch issue | `git checkout -b improve/{id-slug}` |
| Commit | `git commit -F /tmp/commit-msg.txt` |
| Push | `git push -u origin improve/{id-slug}` |
| Buat PR | `gh pr create --base main --head improve/{id-slug} --body-file /tmp/pr-body.md` |
| Merge | `gh pr merge {pr-number} --merge --delete-branch` |
| Hapus branch setelah merge | `git checkout main && git branch -d improve/{id-slug}` |
| Lihat issue list | `gh issue list --label P0 --state open` |