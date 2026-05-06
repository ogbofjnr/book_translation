const levels = ["A1", "A2", "B1", "B2", "C1", "C2"];
const BOOST_PARALLEL = 20;

const state = {
  books: [],
  activeBookId: null,
  bookInstance: null,
  rendition: null,
  currentPageText: "",
  autoTranslateTimer: null,
  translateRunId: 0,
  translateController: null,
  lastAppliedKey: "",
  translationCache: new Map(),
  lastCfi: "",
  userNavigatedAt: 0,
  suppressAutoTranslateUntil: 0,
  savePosTimer: null,
  bootstrapBurstPending: false,
};

const els = {
  appShell: document.getElementById("appShell"),
  sidebar: document.getElementById("sidebar"),
  toggleSidebarBtn: document.getElementById("toggleSidebarBtn"),
  userInput: document.getElementById("userInput"),
  uploadForm: document.getElementById("uploadForm"),
  dropZone: document.getElementById("dropZone"),
  bookFileInput: document.getElementById("bookFileInput"),
  booksList: document.getElementById("booksList"),
  activeBookTitle: document.getElementById("activeBookTitle"),
  statusText: document.getElementById("statusText"),
  reader: document.getElementById("reader"),
  languageInput: document.getElementById("languageInput"),
  thresholdSlider: document.getElementById("thresholdSlider"),
  thresholdLabel: document.getElementById("thresholdLabel"),
  autoTranslateToggle: document.getElementById("autoTranslateToggle"),
  fontSizeSlider: document.getElementById("fontSizeSlider"),
  fontSizeLabel: document.getElementById("fontSizeLabel"),
  themeSelect: document.getElementById("themeSelect"),
  tocJumpBtn: document.getElementById("tocJumpBtn"),
};

els.toggleSidebarBtn.addEventListener("click", () => {
  els.appShell.classList.toggle("collapsed");
  relayoutReaderSoon();
});

els.thresholdSlider.addEventListener("input", () => {
  els.thresholdLabel.textContent = levels[Number(els.thresholdSlider.value)];
  saveUISettings();
  if (els.autoTranslateToggle.checked) scheduleAutoTranslate(250);
});

els.languageInput.addEventListener("change", () => {
  saveUISettings();
  if (els.autoTranslateToggle.checked) scheduleAutoTranslate(250);
});

els.fontSizeSlider.addEventListener("input", () => {
  els.fontSizeLabel.textContent = `${els.fontSizeSlider.value}%`;
  saveUISettings();
  applyReaderTheme();
});

els.themeSelect.addEventListener("change", () => {
  saveUISettings();
  applyReaderTheme();
});

els.autoTranslateToggle.addEventListener("change", () => {
  saveUISettings();
  if (els.autoTranslateToggle.checked) {
    // Force immediate translation for the currently visible page when toggled on.
    state.lastAppliedKey = "";
    state.userNavigatedAt = Date.now();
    state.bootstrapBurstPending = true;
    scheduleAutoTranslate(50);
  } else {
    if (state.autoTranslateTimer) clearTimeout(state.autoTranslateTimer);
    if (state.translateController) state.translateController.abort();
    els.statusText.textContent = "Auto translate disabled.";
  }
});

els.tocJumpBtn.addEventListener("click", async () => {
  await jumpToTOC();
});

els.uploadForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const file = els.bookFileInput.files?.[0];
  if (!file) return;
  await uploadBookFile(file);
});

els.bookFileInput.addEventListener("change", async () => {
  const file = els.bookFileInput.files?.[0];
  if (!file) return;
  await uploadBookFile(file);
});

["dragenter", "dragover"].forEach((evt) => {
  els.dropZone.addEventListener(evt, (e) => {
    e.preventDefault();
    e.stopPropagation();
    els.dropZone.classList.add("dragOver");
  });
});

["dragleave", "drop"].forEach((evt) => {
  els.dropZone.addEventListener(evt, (e) => {
    e.preventDefault();
    e.stopPropagation();
    els.dropZone.classList.remove("dragOver");
  });
});

els.dropZone.addEventListener("drop", async (e) => {
  const files = e.dataTransfer?.files;
  if (!files || !files.length) return;
  const file = files[0];
  await uploadBookFile(file);
});

async function translateCurrentPage(mode = "visible") {
  if (!state.rendition) return;
  const cfi = currentLocationKey();
  const translationKey = `${state.activeBookId}|${cfi}|${els.languageInput.value.trim()}|${levels[Number(els.thresholdSlider.value)]}`;
  if (tryApplyCachedTranslation(translationKey)) {
    state.bootstrapBurstPending = false;
    state.lastAppliedKey = translationKey;
    els.statusText.textContent = "Page translated (cached).";
    return;
  }
  if (translationKey === state.lastAppliedKey) {
    return;
  }
  const nodes = collectTranslatableTextNodes();
  if (!nodes.length) {
    els.statusText.textContent = "No translatable text on current page.";
    return;
  }
  const cacheMiss = !state.translationCache.has(translationKey);
  const useBootstrapBurst = mode === "visible" && (state.bootstrapBurstPending || cacheMiss);
  els.statusText.textContent = useBootstrapBurst
    ? `Fast startup translate (parallel x${BOOST_PARALLEL})...`
    : "Translating page in one request...";
  if (state.translateController) {
    state.translateController.abort();
  }
  state.translateController = new AbortController();
  const controller = state.translateController;

  if (useBootstrapBurst) {
    const ok = await translatePageParallelBurst(nodes, controller.signal, cfi, translationKey, BOOST_PARALLEL);
    if (!ok) return;
    state.bootstrapBurstPending = false;
    state.suppressAutoTranslateUntil = Date.now() + 2500;
    state.lastAppliedKey = translationKey;
    els.statusText.textContent = "Page translated (fast burst).";
    return;
  }

  const delimiter = "<<<NODE_BREAK_V1>>>";
  const separator = `\n${delimiter}\n`;
  const combined = nodes.map((n) => (n.nodeValue || "")).join(separator);
  const translatedCombined = await translateChunkText(combined, controller.signal, delimiter);
  if (translatedCombined == null) return;
  if (cfi !== currentLocationKey()) return;

  const translatedParts = splitByDelimiterLoose(translatedCombined, delimiter);
  if (translatedParts.length !== nodes.length) {
    // Best-effort fallback: strict split first, then skip only if still mismatched.
    const strict = translatedCombined.split(separator);
    if (strict.length === nodes.length) {
      applyTranslatedParts(nodes, strict);
      cacheTranslatedPage(translationKey, strict.map((x) => (x || "")));
      state.suppressAutoTranslateUntil = Date.now() + 2500;
      state.lastAppliedKey = translationKey;
      els.statusText.textContent = "Page translated.";
      return;
    }
    els.statusText.textContent = "Translate mismatch (delimiter changed).";
    return;
  }
  const cached = applyTranslatedParts(nodes, translatedParts);
  cacheTranslatedPage(translationKey, cached);

  // Translating text can trigger pagination relocation; suppress re-trigger loop.
  state.suppressAutoTranslateUntil = Date.now() + 2500;
  state.lastAppliedKey = translationKey;
  els.statusText.textContent = "Page translated.";
}

async function translatePageParallelBurst(nodes, signal, cfi, translationKey, parallel = 10) {
  const { chunks, units } = buildSentenceChunksForPage(nodes, 10);
  if (!chunks.length) return false;
  const n = chunks.length;
  const translatedUnits = new Array(units.length);
  let nextIdx = 0;
  let done = 0;

  async function worker() {
    while (true) {
      if (signal.aborted) return false;
      const i = nextIdx++;
      if (i >= n) return true;
      if (cfi !== currentLocationKey()) return false;
      const chunk = chunks[i];
      const delimiter = "<<<SENT_BREAK_V1>>>";
      const separator = `\n${delimiter}\n`;
      const src = chunk.units.map((u) => u.text).join(separator);
      const out = await translateChunkText(src, signal, delimiter);
      if (out == null) return false;
      let parts = splitByDelimiterLoose(out, delimiter);
      if (parts.length !== chunk.units.length) {
        const strict = out.split(separator);
        if (strict.length === chunk.units.length) {
          parts = strict;
        } else {
          // Fallback: keep original unit texts if model broke delimiters.
          parts = chunk.units.map((u) => u.text);
        }
      }
      for (let k = 0; k < chunk.units.length; k++) {
        const u = chunk.units[k];
        const v = (parts[k] || "");
        translatedUnits[u.unitIdx] = v || u.text;
      }
      done++;
      els.statusText.textContent = `Fast startup translate... (${done}/${n})`;
    }
  }

  const workers = [];
  const wCount = Math.max(1, Math.min(parallel, n));
  for (let i = 0; i < wCount; i++) workers.push(worker());
  const results = await Promise.all(workers);
  if (results.some((x) => x === false)) return false;
  if (cfi !== currentLocationKey()) return false;

  const cached = applyTranslatedUnitsToNodes(nodes, units, translatedUnits);
  cacheTranslatedPage(translationKey, cached);
  return true;
}

// Background preload path: always single request (no turbo burst).
async function preloadCurrentPage() {
  return translateCurrentPage("preload");
}

function currentUser() {
  return els.userInput.value.trim() || "local-user";
}

async function loadBooks() {
  const resp = await fetch(`/api/books?user=${encodeURIComponent(currentUser())}`);
  const data = await resp.json();
  state.books = Array.isArray(data) ? data : [];
  renderBookList();
}

async function uploadBookFile(file) {
  const name = (file?.name || "").toLowerCase();
  if (!name.endsWith(".epub")) {
    els.statusText.textContent = "Only .epub files are supported.";
    return;
  }
  const body = new FormData();
  body.append("book", file);
  els.statusText.textContent = "Uploading book...";
  const resp = await fetch(`/api/books/upload?user=${encodeURIComponent(currentUser())}`, {
    method: "POST",
    body,
  });
  if (!resp.ok) {
    els.statusText.textContent = `Upload failed: ${await resp.text()}`;
    return;
  }
  els.bookFileInput.value = "";
  await loadBooks();
  els.statusText.textContent = "Upload complete.";
}

function renderBookList() {
  els.booksList.innerHTML = "";
  for (const b of state.books) {
    const li = document.createElement("li");
    li.className = "bookItem";
    const displayName = b.display_name || b.title || b.file_name;

    const openBtn = document.createElement("button");
    openBtn.className = "bookOpenBtn";
    openBtn.textContent = displayName;
    openBtn.addEventListener("click", () => openBook(b.id, displayName));

    const right = document.createElement("div");
    right.className = "bookMeta";

    const percent = document.createElement("span");
    percent.className = "bookPercent";
    percent.textContent = `${getReadPercent(b.id)}%`;

    const del = document.createElement("button");
    del.className = "bookDeleteBtn";
    del.textContent = "✕";
    del.title = "Delete book";
    del.addEventListener("click", async (e) => {
      e.preventDefault();
      e.stopPropagation();
      await deleteBook(b.id);
    });

    right.appendChild(percent);
    right.appendChild(del);

    li.appendChild(openBtn);
    li.appendChild(right);
    els.booksList.appendChild(li);
  }
}

async function deleteBook(bookId) {
  const resp = await fetch(`/api/books/${encodeURIComponent(bookId)}?user=${encodeURIComponent(currentUser())}`, {
    method: "DELETE",
  });
  if (!resp.ok) {
    els.statusText.textContent = `Delete failed: ${await resp.text()}`;
    return;
  }
  localStorage.removeItem(progressKey(bookId));
  if (state.activeBookId === bookId) {
    state.activeBookId = null;
    if (state.rendition) {
      try { state.rendition.destroy(); } catch (_) {}
    }
    if (state.bookInstance) {
      try { state.bookInstance.destroy(); } catch (_) {}
    }
    els.reader.innerHTML = "";
    els.activeBookTitle.textContent = "No book selected";
    els.statusText.textContent = "Book deleted.";
  }
  await loadBooks();
}

async function openBook(bookId, title) {
  state.activeBookId = bookId;
  els.activeBookTitle.textContent = title;
  els.statusText.textContent = "";
  state.currentPageText = "";
  state.lastAppliedKey = "";
  if (state.translateController) {
    state.translateController.abort();
    state.translateController = null;
  }

  if (state.rendition) {
    try {
      state.rendition.destroy();
    } catch (_) {}
  }
  if (state.bookInstance) {
    try {
      state.bookInstance.destroy();
    } catch (_) {}
  }
  els.reader.innerHTML = "";

  try {
    const url = `/api/books/${encodeURIComponent(bookId)}/download?user=${encodeURIComponent(currentUser())}`;
    const resp = await fetch(url);
    if (!resp.ok) {
      els.statusText.textContent = `Error loading book: ${await resp.text()}`;
      return;
    }
    const data = await resp.arrayBuffer();
    state.bookInstance = ePub(data);
    await ensureLocationsGenerated();
    state.rendition = state.bookInstance.renderTo("reader", {
      width: "100%",
      height: "100%",
      spread: "none",
    });
    applyReaderTheme();
    // EPUB render updates happen inside iframes; wait for render/relocation before extracting text.
    const onPageChanged = () => {
      setTimeout(async () => {
        const now = Date.now();
        await refreshCurrentPageTextSoon();
        const cfiNow = currentLocationKey();
        if (!cfiNow || cfiNow === state.lastCfi) return;
        state.lastCfi = cfiNow;
        state.lastAppliedKey = "";

        // Ignore relocated events triggered by our own text mutations.
        if (now < state.suppressAutoTranslateUntil) {
          return;
        }

        const navigatedRecently = now - state.userNavigatedAt < 1800;
        // Keep progress in sync with the latest reading location.
        saveReadingPositionSoon();
        await saveReadPercent(state.activeBookId);

        const key = currentTranslationKey();
        if (tryApplyCachedTranslation(key)) {
          state.bootstrapBurstPending = false;
          state.lastAppliedKey = key;
          els.statusText.textContent = "Page translated (cached).";
          return;
        }
        // Auto-translate only when user explicitly navigated recently (or on first open).
        if (els.autoTranslateToggle.checked && (navigatedRecently || state.userNavigatedAt === 0)) {
          scheduleAutoTranslate();
        } else if (!els.autoTranslateToggle.checked) {
          els.statusText.textContent = "Auto translate is off.";
        }
      }, 80);
    };
    state.rendition.on("relocated", onPageChanged);
    state.rendition.on("rendered", (_section, contents) => {
      try {
        const doc = contents?.document;
        if (!doc) return;
        doc.addEventListener("keydown", (e) => {
          const tag = (e.target?.tagName || "").toLowerCase();
          if (tag === "input" || tag === "textarea" || tag === "select") return;
          if (!state.rendition) return;
          if (e.key === "ArrowRight") {
            e.preventDefault();
            state.userNavigatedAt = Date.now();
            state.rendition.next();
          } else if (e.key === "ArrowLeft") {
            e.preventDefault();
            state.userNavigatedAt = Date.now();
            state.rendition.prev();
          }
        });
      } catch (_) {}
    });

    const savedCfi = getSavedReadingPosition();
    if (savedCfi) {
      await state.rendition.display(savedCfi);
    } else {
      await state.rendition.display();
    }
    await refreshCurrentPageTextSoon();
    if (!state.currentPageText.trim()) {
      els.statusText.textContent = "Book opened, but page text is still loading...";
    }
    state.lastCfi = currentLocationKey();
    const key = currentTranslationKey();
    if (tryApplyCachedTranslation(key)) {
      state.bootstrapBurstPending = false;
      state.lastAppliedKey = key;
      els.statusText.textContent = "Page translated (cached).";
    } else if (els.autoTranslateToggle.checked) {
      state.userNavigatedAt = 0;
      state.bootstrapBurstPending = true;
      scheduleAutoTranslate(200);
    } else {
      els.statusText.textContent = "Book opened. Auto translate is off.";
    }
  } catch (err) {
    els.statusText.textContent = `Open book failed: ${String(err)}`;
  }
}

async function jumpToTOC() {
  if (!state.bookInstance || !state.rendition) return;
  try {
    const nav = await state.bookInstance.loaded.navigation;
    const toc = flattenNav(nav?.toc || []);
    let target = toc.find((x) => {
      const l = (x.label || "").toLowerCase();
      const h = (x.href || "").toLowerCase();
      return l.includes("table of contents") || l.includes("contents") || l.includes("toc") || h.includes("toc") || h.includes("contents");
    });
    if (!target && toc.length) target = toc[0];
    if (!target?.href) {
      els.statusText.textContent = "TOC entry not found in this EPUB.";
      return;
    }
    state.userNavigatedAt = Date.now();
    await state.rendition.display(target.href);
    els.statusText.textContent = "Opened table of contents.";
  } catch (err) {
    els.statusText.textContent = `TOC open failed: ${String(err)}`;
  }
}

function flattenNav(items, out = []) {
  for (const it of items || []) {
    out.push({ label: it.label || "", href: it.href || "" });
    if (it.subitems?.length) flattenNav(it.subitems, out);
  }
  return out;
}

function getVisibleText() {
  if (!state.rendition) return "";

  const contents = state.rendition.getContents?.() || [];
  const chunks = [];
  for (const c of contents) {
    // epubjs exposes iframe document as `document` in most builds
    const txt = c?.document?.body?.innerText || "";
    if (txt.trim()) chunks.push(txt.trim());
  }

  if (chunks.length) return chunks.join("\n\n");

  // Fallback: take innerText from iframe(s) under the reader container.
  const iframes = els.reader.querySelectorAll("iframe");
  for (const iframe of iframes) {
    try {
      const doc = iframe.contentDocument;
      const body = doc?.body;
      const txt = body?.innerText || body?.textContent || "";
      if (txt.trim()) chunks.push(txt.trim());
    } catch (_) {
      // ignore iframe access errors
    }
  }
  return chunks.join("\n\n");
}

async function refreshCurrentPageTextSoon() {
  // Wait for iframe DOM after relocation / navigation.
  await new Promise((r) => setTimeout(r, 120));
  state.currentPageText = getVisibleText() || "";
}

function scheduleAutoTranslate(delay = 700) {
  if (!state.rendition) return;
  if (!els.autoTranslateToggle.checked) return;
  if (state.autoTranslateTimer) {
    clearTimeout(state.autoTranslateTimer);
  }
  const runId = ++state.translateRunId;
  state.autoTranslateTimer = setTimeout(async () => {
    if (runId !== state.translateRunId) return;
    await translateCurrentPage("visible");
  }, delay);
}

function collectTranslatableTextNodes() {
  const docs = collectVisibleDocuments();
  const out = [];
  for (const doc of docs) {
    const body = doc?.body;
    if (!body) continue;
    const walker = doc.createTreeWalker(body, NodeFilter.SHOW_TEXT);
    let node = walker.nextNode();
    while (node) {
      const parent = node.parentElement;
      const txt = (node.nodeValue || "").trim();
      if (
        txt &&
        txt.length >= 30 &&
        parent &&
        !isForbiddenNode(parent) &&
        !alreadyAnnotated(txt)
      ) {
        out.push(node);
      }
      node = walker.nextNode();
    }
  }
  // Hard cap for responsiveness on very dense pages.
  return out.slice(0, 28);
}

function collectVisibleDocuments() {
  const docs = [];
  const contents = state.rendition?.getContents?.() || [];
  for (const c of contents) {
    if (c?.document?.body) docs.push(c.document);
  }
  if (docs.length) return docs;

  const iframes = els.reader.querySelectorAll("iframe");
  for (const iframe of iframes) {
    try {
      const d = iframe.contentDocument;
      if (d?.body) docs.push(d);
    } catch (_) {}
  }
  return docs;
}

function isForbiddenNode(el) {
  const tag = (el.tagName || "").toLowerCase();
  if (["script", "style", "code", "pre", "textarea"].includes(tag)) return true;
  // Keep EPUB nav/toc structures untouched.
  if (el.closest?.("nav")) return true;
  return false;
}

function alreadyAnnotated(text) {
  // Heuristic: skip if already has Cyrillic in parentheses.
  return /\([^)а-яёА-ЯЁ]{0,40}[а-яёА-ЯЁ][^)]{0,80}\)/.test(text);
}

async function translateChunkText(text, signal, delimiter) {
  const payload = {
    text,
    target_language: els.languageInput.value.trim() || "Russian",
    threshold: levels[Number(els.thresholdSlider.value)],
    delimiter: delimiter || "",
  };
  const resp = await fetch("/api/annotate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
    signal,
  }).catch((err) => {
    if (err?.name === "AbortError") return null;
    throw err;
  });
  if (!resp) return null;
  const out = await resp.text();
  if (!resp.ok) {
    els.statusText.textContent = `Error: ${out}`;
    return null;
  }
  const parsed = JSON.parse(out);
  return parsed.annotated_text || "";
}

function currentLocationKey() {
  return state.rendition?.currentLocation?.()?.start?.cfi || "";
}

function currentTranslationKey() {
  const cfi = currentLocationKey();
  return `${state.activeBookId}|${cfi}|${els.languageInput.value.trim()}|${levels[Number(els.thresholdSlider.value)]}`;
}

function readingPosKey() {
  if (!state.activeBookId) return "";
  return `liveReader:pos:${currentUser()}:${state.activeBookId}`;
}

function saveReadingPosition() {
  const key = readingPosKey();
  const cfi = currentLocationKey();
  if (!key || !cfi) return;
  localStorage.setItem(key, cfi);
}

function saveReadingPositionSoon(delay = 250) {
  if (state.savePosTimer) clearTimeout(state.savePosTimer);
  state.savePosTimer = setTimeout(() => {
    saveReadingPosition();
  }, delay);
}

function getSavedReadingPosition() {
  const key = readingPosKey();
  if (!key) return "";
  return localStorage.getItem(key) || "";
}

function progressKey(bookId) {
  return `liveReader:progress:${currentUser()}:${bookId}`;
}

function getReadPercent(bookId) {
  const raw = localStorage.getItem(progressKey(bookId));
  if (!raw) return 0;
  const v = Number(raw);
  if (!Number.isFinite(v)) return 0;
  return Math.max(0, Math.min(100, Math.round(v)));
}

async function ensureLocationsGenerated() {
  if (!state.bookInstance) return;
  try {
    if (!state.bookInstance.locations || !state.bookInstance.locations.length()) {
      await state.bookInstance.locations.generate(1200);
    }
  } catch (_) {}
}

async function saveReadPercent(bookId) {
  if (!bookId || !state.bookInstance) return;
  const cfi = currentLocationKey();
  if (!cfi) return;
  try {
    // Ensure locations exist before percentage calculation.
    if (!state.bookInstance.locations || !state.bookInstance.locations.length()) {
      await ensureLocationsGenerated();
      if (!state.bookInstance.locations || !state.bookInstance.locations.length()) {
        return;
      }
    }
    const p = state.bookInstance.locations?.percentageFromCfi?.(cfi);
    if (typeof p !== "number" || !Number.isFinite(p)) return;
    let percent = Math.round(p * 100);
    if (percent === 0 && p > 0) percent = 1;
    localStorage.setItem(progressKey(bookId), String(Math.max(0, Math.min(100, percent))));
    renderBookList();
  } catch (_) {}
}

function saveUISettings() {
  const data = {
    user: currentUser(),
    language: els.languageInput.value.trim() || "Russian",
    thresholdIndex: Number(els.thresholdSlider.value),
    autoTranslate: !!els.autoTranslateToggle.checked,
    fontSize: Number(els.fontSizeSlider.value),
    theme: els.themeSelect.value || "light",
  };
  localStorage.setItem("liveReader:settings", JSON.stringify(data));
}

function loadUISettings() {
  const raw = localStorage.getItem("liveReader:settings");
  if (!raw) return;
  try {
    const cfg = JSON.parse(raw);
    if (cfg.user) els.userInput.value = cfg.user;
    if (cfg.language) els.languageInput.value = cfg.language;
    if (Number.isInteger(cfg.thresholdIndex)) {
      els.thresholdSlider.value = String(Math.max(0, Math.min(5, cfg.thresholdIndex)));
      els.thresholdLabel.textContent = levels[Number(els.thresholdSlider.value)];
    }
    if (typeof cfg.autoTranslate === "boolean") {
      els.autoTranslateToggle.checked = cfg.autoTranslate;
    }
    if (cfg.fontSize) {
      els.fontSizeSlider.value = String(cfg.fontSize);
      els.fontSizeLabel.textContent = `${cfg.fontSize}%`;
    }
    if (cfg.theme) {
      els.themeSelect.value = cfg.theme;
    }
  } catch (_) {}
}

function applyReaderTheme() {
  if (!state.rendition) return;
  const theme = els.themeSelect.value || "light";
  const fontSize = `${els.fontSizeSlider.value || 100}%`;
  const palettes = {
    light: { bg: "#ffffff", text: "#111111" },
    sepia: { bg: "#f4ecd8", text: "#2b2217" },
    dark: { bg: "#111111", text: "#eeeeee" },
  };
  const p = palettes[theme] || palettes.light;
  state.rendition.themes.default({
    body: {
      "background-color": `${p.bg} !important`,
      color: `${p.text} !important`,
      "font-size": `${fontSize} !important`,
      "line-height": "1.55 !important",
    },
    p: {
      color: `${p.text} !important`,
    },
  });
  state.rendition.themes.fontSize(fontSize);
}

els.userInput.addEventListener("change", async () => {
  saveUISettings();
  await loadBooks();
});

window.addEventListener("resize", () => {
  relayoutReaderSoon();
});

document.addEventListener("keydown", (e) => {
  const tag = (e.target?.tagName || "").toLowerCase();
  if (tag === "input" || tag === "textarea" || tag === "select") return;
  if (!state.rendition) return;
  if (e.key === "ArrowRight") {
    e.preventDefault();
    state.userNavigatedAt = Date.now();
    state.rendition.next();
  } else if (e.key === "ArrowLeft") {
    e.preventDefault();
    state.userNavigatedAt = Date.now();
    state.rendition.prev();
  }
});

window.addEventListener("beforeunload", () => {
  saveReadingPosition();
});

function cacheTranslatedPage(pageKey, translatedNodes) {
  if (!pageKey || !translatedNodes?.length) return;
  state.translationCache.set(pageKey, translatedNodes);
}

function tryApplyCachedTranslation(pageKey) {
  const list = state.translationCache.get(pageKey);
  if (!list || !list.length) return false;
  const nodes = collectTranslatableTextNodes();
  if (!nodes.length) return false;
  const n = Math.min(nodes.length, list.length);
  for (let i = 0; i < n; i++) {
    nodes[i].nodeValue = list[i];
  }
  return true;
}

function escapeRegExp(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function splitByDelimiterLoose(text, delimiter) {
  const re = new RegExp(`(?:\\r?\\n)?${escapeRegExp(delimiter)}(?:\\r?\\n)?`, "g");
  return text.split(re);
}

function applyTranslatedParts(nodes, parts) {
  const cached = [];
  for (let i = 0; i < nodes.length; i++) {
    const t = (parts[i] || "");
    if (t.length > 0) {
      nodes[i].nodeValue = t;
      cached.push(t);
    } else {
      cached.push(nodes[i].nodeValue || "");
    }
  }
  return cached;
}

function splitSentencesKeepWhitespace(text) {
  const out = [];
  const re = /[^.!?…]+(?:[.!?…]+)?\s*/g;
  const matches = text.match(re);
  if (!matches) return [text];
  for (const m of matches) {
    if (m) out.push(m);
  }
  return out.length ? out : [text];
}

function buildSentenceChunksForPage(nodes, desiredChunks = 10) {
  const units = [];
  let totalChars = 0;

  for (let nodeIdx = 0; nodeIdx < nodes.length; nodeIdx++) {
    const raw = nodes[nodeIdx].nodeValue || "";
    const sentences = splitSentencesKeepWhitespace(raw);
    for (const s of sentences) {
      const t = s;
      if (!t || !t.trim()) continue;
      const unitIdx = units.length;
      units.push({ unitIdx, nodeIdx, text: t });
      totalChars += t.length;
    }
  }
  if (!units.length) return { chunks: [], units: [] };

  const targetChars = Math.max(240, Math.min(1200, Math.round(totalChars / Math.max(1, desiredChunks))));
  const chunks = [];
  let cur = [];
  let curChars = 0;
  for (const u of units) {
    if (cur.length && curChars >= targetChars) {
      chunks.push({ units: cur });
      cur = [];
      curChars = 0;
    }
    cur.push(u);
    curChars += u.text.length;
  }
  if (cur.length) chunks.push({ units: cur });
  return { chunks, units };
}

function applyTranslatedUnitsToNodes(nodes, units, translatedUnits) {
  const byNode = new Array(nodes.length).fill("").map(() => []);
  for (const u of units) {
    const t = translatedUnits[u.unitIdx];
    byNode[u.nodeIdx].push((t || u.text));
  }
  const cached = [];
  for (let i = 0; i < nodes.length; i++) {
    const merged = byNode[i].join("");
    if (merged) {
      nodes[i].nodeValue = merged;
      cached.push(merged);
    } else {
      cached.push(nodes[i].nodeValue || "");
    }
  }
  return cached;
}

loadUISettings();
loadBooks();

function relayoutReaderSoon() {
  if (!state.rendition) return;
  const applyRelayout = () => {
    try {
      const w = Math.max(320, els.reader.clientWidth || 0);
      const h = Math.max(320, els.reader.clientHeight || 0);
      state.rendition.resize(w, h);
      const cfi = state.rendition.currentLocation?.()?.start?.cfi || "";
      if (cfi) state.rendition.display(cfi);
    } catch (_) {}
  };

  // Run twice to handle delayed layout recalculation after sidebar collapse/expand.
  requestAnimationFrame(() => {
    applyRelayout();
    setTimeout(applyRelayout, 80);
  });
}
