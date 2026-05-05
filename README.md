# book_translation

CLI tool for translating selected EPUB chapters with DeepSeek.
It keeps the original English text and adds translations in parentheses only for words/phrases above the configured CEFR threshold.

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
- `TARGET_ENGLISH_LEVEL` controls CEFR threshold. Only words/phrases above this level are translated (for example: `B1`, `B2`, `C1`).

Example behavior with:

```env
TARGET_LANGUAGE=Russian
TARGET_ENGLISH_LEVEL=B2
```

Input:

```text
She felt a fleeting sense of melancholy, then smiled and kept walking.
```

Possible output:

```text
She felt a fleeting (мимолетное) sense of melancholy (меланхолии), then smiled and kept walking.
```

Input:

```text
He opened the door and sat at the table.
```

Possible output (no changes for simple words):

```text
He opened the door and sat at the table.
```

## Make Commands

- `make list`
  - Shows all EPUB chapters in reading order (`spine`)
  - Marks translation state:
    - `[x]` translated
    - `[ ]` not translated

- `make translate CHAPTER="08_One"`
  - Translates only chapter files whose EPUB path contains substring `08_One`
  - Writes result back to the same source file (in-place)

- `make translate-all`
  - Translates the whole book in one run
  - Warning: this can take a long time on large books
  - Recommended approach: translate chapter-by-chapter with `make translate CHAPTER="..."`

Optional:

- `BOOK="books/Your Book.epub"` to select a specific book file

Examples:

```bash
make list
make list BOOK="books/Your Book.epub"
make translate CHAPTER="08_One"
make translate BOOK="books/Your Book.epub" CHAPTER="12_Five"
make translate-all
make translate-all BOOK="books/Your Book.epub"
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
