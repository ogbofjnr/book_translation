# book_translation

CLI tool for translating selected EPUB chapters with DeepSeek.

The tool works in-place: it updates the original `.epub` file in `books/`.

## Requirements

- Go (same version used in this repo)
- DeepSeek API key
- `make`

## Setup

1. Put your book into `books/` (for example: `books/The Ecliptic (Benjamin Wood).epub`).
2. Create `.env` (or export env var) with:

```env
DEEPSEEK_API_KEY=your_deepseek_api_key_here
TARGET_LANGUAGE=Russian
TARGET_ENGLISH_LEVEL=B2
```

- `TARGET_LANGUAGE` controls translation language inside parentheses (for example: `Russian`, `Spanish`, `German`).
- `TARGET_ENGLISH_LEVEL` controls CEFR threshold. Words above this level are translated (for example: `B1`, `B2`, `C1`).

## Make Commands

- `make list`
  - Shows all EPUB chapters in reading order (`spine`)
  - Marks translation state:
    - `[x]` translated
    - `[ ]` not translated

- `make translate CHAPTER="08_One"`
  - Translates only chapter files whose EPUB path contains substring `08_One`
  - Writes result back to the same source file (in-place)

Optional:

- `BOOK="books/Your Book.epub"` to select a specific book file

Examples:

```bash
make list
make list BOOK="books/Your Book.epub"
make translate CHAPTER="08_One"
make translate BOOK="books/Your Book.epub" CHAPTER="12_Five"
```

## Progress Logs

Before translation starts, tool prints estimated chunk count:

```text
estimated chunks to process: 89
```

During translation:

```text
translating chunk 4 out of 89
chunk done 4 out of 89
```

## Notes

- Translation metadata is stored inside EPUB OPF as `x-translated-chapters`.
- On each run, old temporary `.epub-out-*` files in the target directory are cleaned up automatically.
