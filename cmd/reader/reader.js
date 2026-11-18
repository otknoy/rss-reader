(() => {
  "use strict";

  let pending;
  let contentPollTimer;
  let contentRequestedFor = "";
  let navigationGeneration = 0;
  let currentContext = contextKey(location.href);
  let mobileListScroll = 0;
  let keyboardCursorID = selectedID();
  let pageMovePending = false;
  let unreadGap = null;
  let favoritePending = false;

  function contextKey(raw) {
    const url = new URL(raw, location.href);
    for (const key of ["article", "message", "added", "existing", "skipped"]) url.searchParams.delete(key);
    url.searchParams.sort();
    return url.searchParams.toString();
  }

  function selectedID(root = document) {
    return root.querySelector(".reader[data-reading-id]")?.dataset.readingId || "";
  }

  function showError(message) {
    document.querySelector(".workspace-error")?.remove();
    const notice = document.createElement("div");
    notice.className = "workspace-error";
    notice.setAttribute("role", "alert");
    notice.textContent = message;
    document.body.append(notice);
  }

  async function navigate(raw, options = {}) {
    const url = new URL(raw, location.href);
    if (url.origin !== location.origin) return;
    ++navigationGeneration;
    clearTimeout(contentPollTimer);
    pending?.abort();
    const controller = new AbortController();
    pending = controller;
    document.querySelector(".workspace-error")?.remove();
    document.querySelector("#reader-pane")?.setAttribute("aria-busy", "true");

    try {
      const response = await fetch(url, {
        method: options.method || "GET",
        body: options.body,
        signal: controller.signal,
        credentials: "same-origin",
        headers: { Accept: "text/html" },
      });
      if (!response.ok) {
        const message = await response.text();
        throw new Error(message.length < 300 ? message : "読み込めませんでした。再度お試しください。");
      }
      const html = await response.text();
      if (controller.signal.aborted) return;
      const next = new DOMParser().parseFromString(html, "text/html");
      const nextReader = next.querySelector("#reader-pane");
      const nextList = next.querySelector("#list-pane");
      const nextSidebar = next.querySelector(".sidebar");
      if (!nextReader || !nextList || !nextSidebar) throw new Error("画面を読み込めませんでした。");

      const sameContext = contextKey(response.url) === currentContext;
      const oldReader = document.querySelector("#reader-pane");
      const oldList = document.querySelector("#list-pane");
      const oldID = selectedID();
      const nextID = selectedID(next);
      const readerScroll = oldReader.scrollTop;
      const listScroll = oldList.scrollTop;
      const cursorIndex = articleRows().findIndex(row => row.dataset.articleId === keyboardCursorID);
      if (!oldID && nextID) mobileListScroll = window.scrollY;

      // Selecting an article only replaces the right column, keeping the list's DOM and scroll.
      if (!sameContext || options.refreshList) {
        oldList.replaceWith(nextList);
        nextList.scrollTop = sameContext ? listScroll : 0;
        const sidebar = document.querySelector(".sidebar");
        const sidebarScroll = sidebar.scrollTop;
        sidebar.replaceWith(nextSidebar);
        nextSidebar.scrollTop = sidebarScroll;
      }
      oldReader.replaceWith(nextReader);
      if (options.keepReaderScroll && oldID === nextID) nextReader.scrollTop = readerScroll;
      document.querySelector(".shell").classList.toggle("has-detail", Boolean(nextID));
      for (const row of document.querySelectorAll("[data-article-id]")) {
        const selected = row.dataset.articleId === nextID;
        row.classList.toggle("selected", selected);
        const link = row.querySelector("a[data-workspace-link]");
        if (selected) link.setAttribute("aria-current", "true");
        else link.removeAttribute("aria-current");
        // Forms in the retained list must return to the new selection as well.
        for (const input of row.querySelectorAll('input[name="return_to"]')) {
          input.value = nextList.querySelector('input[name="return_to"]')?.value || response.url;
        }
      }
      // Header forms also retain the current selected article.
      if (sameContext && !options.refreshList) {
        for (const input of document.querySelectorAll('#list-pane input[name="return_to"]')) {
          input.value = nextList.querySelector('input[name="return_to"]')?.value || response.url;
        }
        document.querySelector("#list-pane .notice")?.remove();
        oldList.scrollTop = listScroll;
      }

      const rows = articleRows();
      if (options.markRead && nextID) {
        const row = rows.find(item => item.dataset.articleId === nextID);
        // The unread filter removes the article. Keep its former position as a gap.
        unreadGap = row ? null : {
          id: nextID,
          index: oldList.dataset.page === nextList.dataset.page ? options.readIndex : rows.length,
        };
        setCursor(row, true);
      } else if (options.cursorEdge) {
        unreadGap = null;
        setCursor(options.cursorEdge === "first" ? rows[0] : rows.at(-1), true);
      } else if (!sameContext || nextID !== oldID) {
        unreadGap = null;
        setCursor(rows.find(row => row.dataset.articleId === nextID));
      } else if (unreadGap?.id === nextID) {
        setCursor(null);
      } else {
        setCursor(rows.find(row => row.dataset.articleId === keyboardCursorID) || (cursorIndex >= 0 ? rows[Math.min(cursorIndex, rows.length - 1)] : null));
      }

      currentContext = contextKey(response.url);
      if (options.history === "replace") history.replaceState({}, "", response.url);
      else if (options.history !== "none") history.pushState({}, "", response.url);
      if (!options.keepReaderScroll) {
        (nextID ? nextReader : document.querySelector("#list-pane")).focus({ preventScroll: true });
        if (matchMedia("(max-width: 600px)").matches) window.scrollTo(0, nextID ? 0 : mobileListScroll);
      }
    } catch (error) {
      if (error.name !== "AbortError") showError(error.message || "読み込めませんでした。");
    } finally {
      if (pending === controller) {
        document.querySelector("#reader-pane")?.removeAttribute("aria-busy");
        scheduleContentCheck();
      }
    }
  }

  function scheduleContentCheck() {
    clearTimeout(contentPollTimer);
    const reader = document.querySelector('.reader[data-content-pending="true"]');
    if (!reader) {
      contentRequestedFor = "";
      return;
    }
    const id = reader.dataset.readingId;
    const requestNow = contentRequestedFor !== id;
    contentRequestedFor = id;
    const hadError = reader.dataset.contentError === "true";
    const generation = navigationGeneration;
    contentPollTimer = setTimeout(async () => {
      try {
        const response = await fetch(`/articles/${id}/content-status`, {
          method: requestNow ? "POST" : "GET",
          credentials: "same-origin",
          headers: { Accept: "application/json" },
        });
        if (generation !== navigationGeneration || selectedID() !== id) return;
        if (!response.ok) { scheduleContentCheck(); return; }
        const status = await response.json();
        if (generation !== navigationGeneration || selectedID() !== id) return;
        if (!status.pending || Boolean(status.error) !== hadError) {
          await navigate(location.href, { history: "none", keepReaderScroll: true });
        } else {
          scheduleContentCheck();
        }
      } catch {
        if (generation === navigationGeneration && selectedID() === id) scheduleContentCheck();
      }
    }, requestNow ? 0 : hadError ? 30000 : 5000);
  }

  function articleRows() {
    return Array.from(document.querySelectorAll("#list-pane [data-article-id]"));
  }

  function setCursor(row, scroll = false) {
    const previous = keyboardCursorID;
    keyboardCursorID = row?.dataset.articleId || "";
    for (const item of articleRows()) item.classList.toggle("keyboard-cursor", item === row);
    if (scroll && row) row.scrollIntoView({ block: "nearest", inline: "nearest" });
    const status = document.querySelector("#keyboard-status");
    if (status && previous !== keyboardCursorID) status.textContent = row?.querySelector("h2")?.textContent || "";
  }

  async function moveCursor(delta) {
    if (pageMovePending || favoritePending) return;
    pageMovePending = true;
    try {
      let rows = articleRows();
      const index = rows.findIndex(row => row.dataset.articleId === keyboardCursorID);
      const gap = unreadGap?.id === selectedID() ? unreadGap.index : null;
      let next = gap !== null ? Math.min(gap, rows.length) + (delta > 0 ? 0 : -1) : index < 0 ? 0 : index + delta;
      if (!rows[next]) {
        const direction = delta > 0 ? "next" : "previous";
        const link = document.querySelector(`[data-keyboard-page="${direction}"]`);
        if (!link) return;
        const url = new URL(link.href);
        if (selectedID()) url.searchParams.set("article", selectedID());
        await navigate(url, { cursorEdge: delta > 0 ? "first" : "last", keepReaderScroll: true });
        rows = articleRows();
        next = rows.findIndex(row => row.dataset.articleId === keyboardCursorID);
      }
      const row = rows[next];
      if (!row) return;
      setCursor(row, true);
      const url = new URL(row.querySelector("a[data-workspace-link]").href);
      const form = row.querySelector('input[name="kind"][value="read"]').form;
      await navigate(form.action, {
        method: "POST",
        body: new URLSearchParams({ kind: "read", value: "true", return_to: url.pathname + url.search }),
        refreshList: true,
        markRead: true,
        readIndex: next,
      });
    } finally {
      pageMovePending = false;
    }
  }

  function openOriginal() {
    const scope = document.querySelector(".reader") || articleRows().find(row => row.dataset.articleId === keyboardCursorID);
    scope?.querySelector("[data-original-link]")?.click();
  }

  async function toggleCursorFavorite() {
    if (favoritePending || pageMovePending) return;
    const row = articleRows().find(item => item.dataset.articleId === keyboardCursorID);
    const scope = row || document.querySelector(".reader");
    const form = scope && Array.from(scope.querySelectorAll("form")).find(item => item.querySelector('input[name="kind"]')?.value === "favorite");
    if (!form) return;
    favoritePending = true;
    try {
      await navigate(form.action, {
        method: "POST",
        body: new URLSearchParams(new FormData(form)),
        history: "replace",
        refreshList: true,
        keepReaderScroll: true,
      });
    } finally {
      favoritePending = false;
    }
  }

  document.addEventListener("keydown", event => {
    const target = event.target instanceof Element ? event.target : document.activeElement;
    if (event.defaultPrevented || event.isComposing || event.ctrlKey || event.metaKey || event.altKey) return;
    if (target?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])') || document.querySelector(".add[open]")) return;
    if (event.key === " " && selectedID()) {
      if (target?.closest('button, summary, a, [role="button"]')) return;
      const pane = document.querySelector("#reader-pane");
      if (!pane || getComputedStyle(pane).display === "none") return;
      event.preventDefault();
      pane.scrollBy({
        top: (event.shiftKey ? -1 : 1) * Math.max(80, pane.clientHeight * .8),
        behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
      });
    } else if (!event.shiftKey && (event.key === "j" || event.key === "k")) {
      event.preventDefault();
      void moveCursor(event.key === "j" ? 1 : -1);
    } else if (!event.shiftKey && event.key === "i") {
      event.preventDefault();
      if (!event.repeat) openOriginal();
    } else if (!event.shiftKey && event.key === "f") {
      event.preventDefault();
      if (!event.repeat) void toggleCursorFavorite();
    }
  });

  document.addEventListener("click", (event) => {
    const row = event.target.closest("[data-article-id]");
    if (row) setCursor(row);
    const link = event.target.closest("a[data-workspace-link]");
    if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || link.target) return;
    if (new URL(link.href).origin !== location.origin) return;
    event.preventDefault();
    void navigate(link.href);
  });

  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (!form.closest("#workspace, .sidebar")) return;
    if (form.hasAttribute("data-workspace-search")) {
      event.preventDefault();
      const url = new URL(form.action);
      url.search = new URLSearchParams(new FormData(form)).toString();
      void navigate(url);
    } else if (form.method.toLowerCase() === "post") {
      event.preventDefault();
      void navigate(form.action, {
        method: "POST",
        body: form.enctype === "multipart/form-data" ? new FormData(form) : new URLSearchParams(new FormData(form)),
        history: "replace",
        refreshList: true,
        keepReaderScroll: true,
      });
    }
  });

  addEventListener("popstate", () => void navigate(location.href, { history: "none" }));
  setCursor(articleRows().find(row => row.dataset.articleId === keyboardCursorID));
  scheduleContentCheck();
})();
