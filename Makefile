APP := go run .
INPUT_DIR ?= books
BOOK ?=
CHAPTER ?=
INPUT_ARG := $(if $(BOOK),--input "$(BOOK)",--input-dir "$(INPUT_DIR)")

.PHONY: help list translate

help:
	@echo "Usage:"
	@echo "  make list [BOOK=books/file.epub]"
	@echo "  make translate CHAPTER=\"chapter-substring\" [BOOK=books/file.epub]"

list:
	@$(APP) --list $(INPUT_ARG)

translate:
	@if [ -z "$(CHAPTER)" ]; then echo "Provide CHAPTER substring. Example: make translate CHAPTER=\"chapter_005\""; exit 1; fi
	@$(APP) $(INPUT_ARG) --epub-only "$(CHAPTER)"
