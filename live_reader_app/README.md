# live_reader_app

New standalone localhost MVP for live EPUB reading + on-demand page annotation.

## Features (MVP)

- Upload and list EPUB books per user (`?user=...`).
- Open selected book in reader view.
- Collapsible left menu; reader uses full width when collapsed.
- Choose target language.
- CEFR threshold slider with markers `A1 A2 B1 B2 C1 C2`.
- Translate current visible page text via DeepSeek prompt (no frequency dictionary yet).
- SQLite storage for books metadata.

## Run

1. Create `.env` from `.env.example`.
2. Start:

```bash
make deps
make run
```

3. Open `http://localhost:8080`.

## API

- `GET /api/books?user=<id>`
- `POST /api/books/upload?user=<id>` (`multipart/form-data`, file key: `book`)
- `GET /api/books/{id}/download?user=<id>`
- `POST /api/annotate`

Example annotate body:

```json
{
  "text": "Your visible page text...",
  "target_language": "Russian",
  "threshold": "B2"
}
```
