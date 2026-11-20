# MokiBox Agent Kit

Standar kerja yang dipakai AI agent (dan programmer manusia) yang mengerjakan atau
melanjutkan MokiBox. Tujuannya: **hasil konsisten antar contributor** — siapa pun
yang mengerjakan, output-nya mengikuti pola yang sama.

Folder ini **self-contained dan portable**: salin ke repo/proyek lain, dan adaptasi
bagian yang tidak berlaku.

---

## Struktur

```
.agent-kit/
├── README.md                      # file ini — index + cara pakai
├── WORKING-STANDARDS.md           # kontrak kerja WAJIB (baca pertama)
├── skills/                        # prosedur kerja (skill, verbatim)
│   ├── git-phase-workflow.md      # roadmap fase prdgen
│   ├── git-improvement-workflow.md# issue improvement (1 issue = 1 branch)
│   ├── mokibox-go-shared.md       # konvensi Go + deployment MokiBox
│   ├── zitadel.md                 # provisioning OIDC
│   ├── presigned-object-urls.md   # R2 presign + CORS
│   ├── vps-prod-deploy.md         # deploy ke VPS multi-tenant
│   ├── go-idiomatic.md            # Go idiomatik + testable
│   ├── tdd.md                     # test-driven development
│   ├── code-review.md             # review perubahan sejak fixed point
│   ├── systematic-debugging.md    # 4-phase root cause debugging
│   ├── diagnosing-bugs.md         # diagnosis loop
│   ├── agent-ready-issues.md      # standar issue yang bisa dieksekusi unaided
│   └── handoff.md                 # handoff antar context
└── references/                    # catatan implementasi per fase (arsip + konteks)
    ├── deployment-topology.md     # topologi deploy + gotcha nginx stale-DNS
    ├── fase-10-verification-notes.md
    ├── fase-2-implementation-notes.md
    ├── fase-3-implementation-notes.md
    ├── fase-4-implementation-notes.md
    ├── fase-5-implementation-notes.md
    └── demo-ui.md
```

---

## Cara Pakai (untuk AI agent)

### 1. Baca `WORKING-STANDARDS.md` duluan

Isinya kontrak kerja yang wajib berlaku di setiap sesi: role, source-of-truth
priority, session-start checklist, aturan deviasi,git discipline, kapan update
CONVENTIONS/HANDOFF.

### 2. Ikuti Session Start Checklist

Ringkasnya:

1. Baca planning docs relevan (`planning/`). Kalau missing/stale → flag, jangan
   asumsikan.
2. Baca `CONVENTIONS.md` + `HANDOFF.md`.
3. `git log -1`, `gh pr list`, `gh issue list --state open` — jangan percaya
   snapshot.
4. Load skill yang relevan (index di bawah). Sebutkan gap di pesan pertama.

**Jangan tulis kode sebelum checklist selesai.**

### 3. Pilih skill berdasarkan jenis pekerjaan

| Kamu sedang… | Pakai skill |
|---|---|
| Kerjakan roadmap fase dari `planning/ISSUES.json` (prdgen) | `git-phase-workflow.md` |
| Kerjakan issue dari `planning/ISSUE_IMPROVEMENT.json` / roadmap parity | `git-improvement-workflow.md` |
| Review perubahan sebelum merge | `code-review.md` |
| Debug bug / regresi | `systematic-debugging.md` atau `diagnosing-bugs.md` |
| Menulis unit test handler | `tdd.md` + bagian mock di `mokibox-go-shared.md` |
| Menulis issue baru | `agent-ready-issues.md` |
| Integrasi Zitadel / OIDC | `zitadel.md` |
| R2 presign / CORS / HLS playlist | `presigned-object-urls.md` |
| Deploy ke VPS / aaPanel / cert | `vps-prod-deploy.md` + `references/deployment-topology.md` |
| Serah-terima konteks ke agent berikutnya | `handoff.md` |

### 4. Ikuti aturan workflow dari skill yang dipilih

Skill workflow (git-phase-workflow / git-improvement-workflow) mendefinisikan:
pola branch, format commit, self-check angka, kapan boleh merge. **Jangan
mengarang format sendiri.**

---

## Perbedaan Dua Skill Workflow (penting)

| | `git-phase-workflow` | `git-improvement-workflow` |
|---|---|---|
| Sumber | `planning/ISSUES.json` (prdgen) | `planning/ISSUE_IMPROVEMENT.json` / roadmap parity |
| Unit branch | 1 fase = 1 branch | **1 issue = 1 branch** |
| Id issue | `phase-N.M` | `P0-1`, `P0-2`, … (bebas) |
| Urutan | fase berurutan | **topologi dependensi** (blocked by) |
| Merge | semua issue fase → 1 PR | **1 PR per issue** |

Memilih salah → agent kerjakan dalam branch yang salah / urutan yang salah.
Perhatikan field `phase` + label issue untuk memutuskan.

---

## Prinsip yang Berlaku Universal (ringkasan)

Standar ini bukan khusus MokiBox saja; dipakai ulang di proyek lain dengan
penyesuaian nama file. Prinsip intinya:

- Source of truth berjenjang; kalau konflik, sumber tertinggi menang dan konfliknya
  di-mention.
- Spec gap di-patch inline, bukan di-cat sebagai deviasi.
- Deviasi hanya untuk blocker; kalau di-cat, acceptance line tetap terlihat.
- Bug saat eksekusi → STOP, kumpulkan bukti, tawarkan opsi, tunggu keputusan.
- External integration → capture payload live sebelum menulis handler yang
  membacanya.
- Smoke lewat endpoint asli, bukan helper/DB direct.
- Integration script harus rerunnable (reset state, buktikan 2x).
- Merge = keputusan manusia, tidak pernah agent self-merge.
- Angka di self-report wajib di-grep ulang, bukan taken from issue body.
- Deploy-infra di branch khusus, bukan `main`.

(Rincian lengkap + konteksnya: `WORKING-STANDARDS.md`)

---

## Maintenance

- Skill ini **universal** (tidak terikat MokiBox) — kalau dipakai di proyek lain,
  ganti path `planning/`, nama file repo, dan referensi tool-specific.
- Skill `mokibox-go-shared.md`, `zitadel.md`, `presigned-object-urls.md`,
  `vps-prod-deploy.md` **khusus MokiBox** — adaptasi kalau dipindah.
- `references/fase-*.md` adalah **arsip konteks** — pertahankan untuk
  incumbency/lessons, tapi bukan prosedur aktif.
- Tambah referensi baru? Taruh di `references/`, update index di README ini.

---

*Kit ini disalin dari working memory owner project dan skill library Hermes,
di-snapshot 2026-10-04 untuk standarisasi lintas contributor.*