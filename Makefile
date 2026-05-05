APP := go run .
INPUT_DIR ?= books
BOOK ?=
CHAPTER ?=
INPUT_ARG := $(if $(BOOK),--input "$(BOOK)",--input-dir "$(INPUT_DIR)")

.PHONY: help list translate translate-all

help:
	@echo "Usage:"
	@echo "  make list [BOOK=books/file.epub]"
	@echo "  make translate CHAPTER=\"chapter-substring\" [BOOK=books/file.epub]"
	@echo "  make translate-all [BOOK=books/file.epub]"
	@echo ""
	@echo "Note: translate-all can take a long time. Recommended: translate by chapters."

list:
	@$(APP) --list $(INPUT_ARG)

translate:
	@if [ -z "$(CHAPTER)" ]; then echo "Provide CHAPTER substring. Example: make translate CHAPTER=\"chapter_005\""; exit 1; fi
	@$(APP) $(INPUT_ARG) --epub-only "$(CHAPTER)"

translate-all:
	@echo "Warning: full-book translation can take a long time."
	@echo "Tip: recommended flow is chapter-by-chapter with make translate CHAPTER=\"...\""
	@$(APP) $(INPUT_ARG)
