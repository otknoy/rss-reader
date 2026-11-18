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
  const retainedReadRows = new Map();
  let favoritePending = false;

  const folderPreferencesKey = "rss-reader:folders";
  let folderOrder = [];
  const pendingFolderStates = new Map();
  const pendingFolderKey = "rss-reader:pending-folder-states";
  try {
    const saved = JSON.parse(localStorage.getItem(folderPreferencesKey));
    if (Array.isArray(saved?.order)) folderOrder = saved.order;
  } catch {
    // Preferences remain usable in this tab when browser storage is unavailable.
  }

  function saveFolderPreferences() {
    try {
      localStorage.setItem(folderPreferencesKey, JSON.stringify({
        order: folderOrder,
      }));
    } catch {
      // Keep the current tab's preferences in memory.
    }
  }

  function applyFolderOrder(sidebar) {
    const rank = new Map(folderOrder.map((id, index) => [id, index]));
    for (const list of sidebar.querySelectorAll("[data-folder-order-list]")) {
      const folders = [...list.children].filter(folder => folder.hasAttribute("data-sortable-folder"));
      folders.sort((a, b) =>
        (rank.get(a.dataset.sortableFolder) ?? folderOrder.length) -
        (rank.get(b.dataset.sortableFolder) ?? folderOrder.length));
      folders.forEach((folder, index) => {
        if (list.children[index] !== folder) list.insertBefore(folder, list.children[index]);
        for (const button of folder.querySelectorAll("[data-folder-step]")) {
          button.disabled = button.dataset.folderStep === "-1" ? index === 0 : index === folders.length - 1;
        }
      });
    }
  }

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
    (document.querySelector(".subscription-manager[open]") || document.body).append(notice);
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
      await Promise.all([...pendingFolderStates.values()].map(state => state.done));
      if (controller.signal.aborted) return;
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
      const nextContext = contextKey(response.url);
      const sameContext = nextContext === currentContext;
      const change = options.stateChange;
      if (sameContext && change?.kind === "favorite" && urlView(response.url) !== "favorites") {
        // The result set is unchanged. Keep the rows, keyboard cursor and scroll in place.
        for (const article of document.querySelectorAll("[data-article-id], .reader[data-reading-id]")) {
          if ((article.dataset.articleId || article.dataset.readingId) === change.id) {
            updateFavoriteButton(article, change.value === "true");
          }
        }
        updateRetainedFavorites(change);
        return;
      }
      const next = new DOMParser().parseFromString(html, "text/html");
      const nextReader = next.querySelector("#reader-pane");
      const nextList = next.querySelector("#list-pane");
      const nextSidebar = next.querySelector(".sidebar");
      if (!nextReader || !nextList || !nextSidebar) throw new Error("画面を読み込めませんでした。");

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
        restoreReadRows(oldList, nextList, nextContext, response.url, options.stateChange);
        oldList.replaceWith(nextList);
        nextList.scrollTop = sameContext ? listScroll : 0;
        const sidebar = document.querySelector(".sidebar");
        const sidebarScroll = sidebar.scrollTop;
        restoreFolders(nextSidebar);
        const manager = sidebar.querySelector(".subscription-manager[open]");
        const managerScroll = manager?.scrollTop;
        const paneScroll = new Map(Array.from(manager?.querySelectorAll("[data-manager-pane]") || [],
          pane => [pane.dataset.managerPane, pane.scrollTop]));
        manager?.close();
        sidebar.replaceWith(nextSidebar);
        if (manager) {
          const nextManager = nextSidebar.querySelector(".subscription-manager");
          nextManager.showModal();
          nextManager.scrollTop = managerScroll;
          for (const pane of nextManager.querySelectorAll("[data-manager-pane]")) {
            pane.scrollTop = paneScroll.get(pane.dataset.managerPane) ?? 0;
          }
        }
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
        setCursor(row, true);
      } else if (options.cursorEdge) {
        setCursor(options.cursorEdge === "first" ? rows[0] : rows.at(-1), true);
      } else if (!sameContext || nextID !== oldID) {
        setCursor(rows.find(row => row.dataset.articleId === nextID));
      } else {
        setCursor(rows.find(row => row.dataset.articleId === keyboardCursorID) || (cursorIndex >= 0 ? rows[Math.min(cursorIndex, rows.length - 1)] : null));
      }

      currentContext = nextContext;
      if (options.history === "replace") history.replaceState({}, "", response.url);
      else if (options.history !== "none") history.pushState({}, "", response.url);
      if (!options.keepReaderScroll && !document.querySelector(".subscription-manager[open]")) {
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

  function urlView(url) {
    return new URL(url).searchParams.get("view");
  }

  function updateFavoriteButton(article, favorite) {
    const button = article?.querySelector(".star");
    if (!button) return;
    button.form.elements.namedItem("value").value = String(!favorite);
    button.classList.toggle("favorite", favorite);
    button.setAttribute("aria-pressed", String(favorite));
    button.setAttribute("aria-label", favorite ? "お気に入りを解除" : "お気に入りに登録");
    button.textContent = favorite ? "★" : "☆";
  }

  function updateRetainedFavorites(change) {
    for (const rows of retainedReadRows.values()) {
      updateFavoriteButton(rows.get(change.id)?.row, change.value === "true");
    }
  }

  function restoreReadRows(oldList, nextList, context, url, change) {
    const retained = retainedReadRows.get(context) || new Map();
    if (change?.kind === "read" && change.value === "false") {
      for (const rows of retainedReadRows.values()) rows.delete(change.id);
    }
    if (change?.kind === "favorite") updateRetainedFavorites(change);
    if (urlView(url) !== "unread") return;
    if (context === currentContext && change?.kind === "read" && change.value === "true") {
      const rows = Array.from(oldList.querySelectorAll("[data-article-id]"));
      const index = rows.findIndex(row => row.dataset.articleId === change.id);
      if (index >= 0) {
        const row = rows[index].cloneNode(true);
        row.classList.add("read");
        const form = row.querySelector('input[name="kind"][value="read"]').form;
        form.elements.namedItem("value").value = "false";
        form.querySelector("button").textContent = "未読に戻す";
        retained.set(change.id, { row, index });
        retainedReadRows.set(context, retained);
      }
    }

    const rows = Array.from(nextList.querySelectorAll("[data-article-id]"));
    const empty = nextList.querySelector(".empty");
    const end = nextList.querySelector(".pagination, .shortcuts-help");
    for (const [id, { row, index }] of Array.from(retained).sort((a, b) => a[1].index - b[1].index)) {
      if (rows.some(item => item.dataset.articleId === id)) continue;
      const copy = row.cloneNode(true);
      nextList.insertBefore(copy, rows[index] || empty || end);
      rows.splice(index, 0, copy);
    }
    if (rows.length) empty?.remove();
  }

  function stateChange(form, data) {
    const id = form.closest("[data-article-id], [data-reading-id]")?.dataset;
    return { id: id?.articleId || id?.readingId, kind: data.get("kind"), value: data.get("value") };
  }

  function articleRows() {
    return Array.from(document.querySelectorAll("#list-pane [data-article-id]"));
  }

  function scrollToCursor(rows, index) {
    const row = rows[index];
    const pane = row.closest("#list-pane");
    if (pane.clientHeight === 0) return;
    if (getComputedStyle(pane).overflowY === "visible") {
      row.scrollIntoView({ block: "nearest", inline: "nearest" });
      return;
    }

    // Keep five titles on either side when the list pane has enough room.
    const titles = rows.map(item => item.querySelector("h2").getBoundingClientRect());
    let first = Math.max(0, index - 5);
    let last = Math.min(rows.length - 1, index + 5);
    while (titles[last].bottom - titles[first].top > pane.clientHeight && first < last) {
      if (index - first > last - index) first++;
      else last--;
    }
    const top = pane.getBoundingClientRect().top + pane.clientTop;
    const bottom = top + pane.clientHeight;
    if (titles[first].top < top) pane.scrollTop += Math.floor(titles[first].top - top);
    else if (titles[last].bottom > bottom) pane.scrollTop += Math.ceil(titles[last].bottom - bottom);
  }

  function setCursor(row, scroll = false) {
    const previous = keyboardCursorID;
    keyboardCursorID = row?.dataset.articleId || "";
    const rows = articleRows();
    for (const item of rows) item.classList.toggle("keyboard-cursor", item === row);
    if (scroll && row) scrollToCursor(rows, rows.indexOf(row));
    const status = document.querySelector("#keyboard-status");
    if (status && previous !== keyboardCursorID) status.textContent = row?.querySelector("h2")?.textContent || "";
  }

  async function openArticle(row, href) {
    setCursor(row, true);
    const url = new URL(href);
    const form = row.querySelector('input[name="kind"][value="read"]').form;
    await navigate(form.action, {
      method: "POST",
      body: new URLSearchParams({ kind: "read", value: "true", return_to: url.pathname + url.search }),
      refreshList: true,
      markRead: true,
      stateChange: { id: row.dataset.articleId, kind: "read", value: "true" },
    });
  }

  async function moveCursor(delta) {
    if (pageMovePending || favoritePending) return;
    pageMovePending = true;
    try {
      let rows = articleRows();
      const index = rows.findIndex(row => row.dataset.articleId === keyboardCursorID);
      let next = index < 0 ? 0 : index + delta;
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
      await openArticle(row, row.querySelector("a[data-workspace-link]").href);
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
    const body = new URLSearchParams(new FormData(form));
    favoritePending = true;
    try {
      await navigate(form.action, {
        method: "POST",
        body,
        stateChange: stateChange(form, body),
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
    if (target?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])') || document.querySelector(".subscription-manager[open]")) return;
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

  function restoreFolders(sidebar) {
    applyFolderOrder(sidebar);
    for (const folder of sidebar.querySelectorAll("[data-sidebar-folder]")) {
      const pendingState = pendingFolderStates.get(folder.dataset.sidebarFolder);
      if (pendingState) folder.open = pendingState.open;
    }
  }

  function rememberPendingFolderStates() {
    try {
      if (!pendingFolderStates.size) sessionStorage.removeItem(pendingFolderKey);
      else sessionStorage.setItem(pendingFolderKey, JSON.stringify(
        [...pendingFolderStates].map(([id, state]) => [id, state.open])));
    } catch {
      // DB writes still work when session storage is unavailable.
    }
  }

  async function saveFolderState(id, state) {
    try {
      const response = await fetch(`/folders/open/${id}`, {
        method: "POST",
        body: new URLSearchParams({ open: String(state.open) }),
        keepalive: true,
      });
      if (!response.ok) throw new Error("保存に失敗しました");
    } catch {
      showError("フォルダーの開閉状態を保存できませんでした。");
    } finally {
      if (pendingFolderStates.get(id) === state) {
        pendingFolderStates.delete(id);
        rememberPendingFolderStates();
      }
    }
  }

  // Persist only user toggles, not native toggle events from rendering HTML.
  document.addEventListener("click", event => {
    const summary = event.target.closest("details[data-sidebar-folder] > summary");
    if (!summary || event.target.closest("a") || event.defaultPrevented) return;
    event.preventDefault();
    const folder = summary.parentElement;
    const id = folder.dataset.sidebarFolder;
    folder.open = !folder.open;
    const previous = pendingFolderStates.get(id);
    const state = { open: folder.open };
    pendingFolderStates.set(id, state);
    rememberPendingFolderStates();
    state.done = previous
      ? previous.done.then(() => saveFolderState(id, state))
      : saveFolderState(id, state);
  });

  document.addEventListener("close", event => {
    if (event.target.matches?.(".subscription-manager") && event.target.isConnected) {
      event.target.closest(".sidebar").querySelector("[data-manage-feeds]")?.focus();
    }
  }, true);

  document.addEventListener("click", async (event) => {
    const orderButton = event.target.closest("[data-folder-step]");
    if (orderButton) {
      const folder = orderButton.closest("[data-sortable-folder]");
      const folders = [...folder.parentElement.querySelectorAll(":scope > [data-sortable-folder]")];
      const from = folders.indexOf(folder);
      const to = from + Number(orderButton.dataset.folderStep);
      if (!folders[to]) return;
      [folders[from], folders[to]] = [folders[to], folders[from]];
      folderOrder = folders.map(item => item.dataset.sortableFolder);
      const sidebar = folder.closest(".sidebar");
      applyFolderOrder(sidebar);
      saveFolderPreferences();
      (orderButton.disabled ? folder.querySelector("[data-folder-step]:not(:disabled)") : orderButton)?.focus();
      return;
    }
    const managerButton = event.target.closest("[data-manage-feeds], [data-close-manager]");
    if (managerButton) {
      const manager = managerButton.closest(".sidebar").querySelector(".subscription-manager");
      if (managerButton.hasAttribute("data-manage-feeds")) manager.showModal();
      else manager.close();
      return;
    }
    const row = event.target.closest("[data-article-id]");
    let link = event.target.closest("a[data-workspace-link]");
    if (!link && row && !event.target.closest(".actions")) {
      link = row.querySelector("a[data-workspace-link]");
    }
    if (!link) {
      if (row) setCursor(row);
      return;
    }
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || link.target) return;
    if (new URL(link.href).origin !== location.origin) return;
    event.preventDefault();
    if (!row) {
      await navigate(link.href);
      return;
    }
    if (pageMovePending || favoritePending) return;
    pageMovePending = true;
    try {
      await openArticle(row, link.href);
    } finally {
      pageMovePending = false;
    }
  });

  let draggedFeed = null;
  let dropTarget = null;

  function highlightDropTarget(target) {
    dropTarget?.classList.remove("drop-target");
    dropTarget = target;
    dropTarget?.classList.add("drop-target");
  }

  document.addEventListener("dragstart", event => {
    const link = event.target.closest("[data-feed-drag]");
    if (!link) return;
    draggedFeed = link;
    link.classList.add("dragging");
    event.dataTransfer.setData("text/plain", link.textContent.trim());
    event.dataTransfer.effectAllowed = "move";
  });

  document.addEventListener("dragover", event => {
    if (!draggedFeed) return;
    const target = event.target.closest("[data-folder-drop]");
    highlightDropTarget(target);
    if (!target) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = "move";
  });

  document.addEventListener("dragend", () => {
    draggedFeed?.classList.remove("dragging");
    draggedFeed = null;
    highlightDropTarget(null);
  });

  document.addEventListener("drop", event => {
    if (!draggedFeed) return;
    const target = event.target.closest("[data-folder-drop]");
    const source = draggedFeed;
    source.classList.remove("dragging");
    draggedFeed = null;
    highlightDropTarget(null);
    if (!target) return;
    event.preventDefault();
    const originalFolder = source.closest(".folder-card") || source.closest("details")?.querySelector("[data-folder-drop]");
    if (originalFolder?.dataset.folderDrop === target.dataset.folderDrop) return;
    void navigate(`/folders/move/${source.dataset.feedDrag}`, {
      method: "POST",
      body: new URLSearchParams({
        folder: target.dataset.folderDrop,
        return_to: location.pathname + location.search,
      }),
      history: "replace",
      refreshList: true,
      keepReaderScroll: true,
    });
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
      const data = new FormData(form);
      void navigate(form.action, {
        method: "POST",
        body: form.enctype === "multipart/form-data" ? data : new URLSearchParams(data),
        stateChange: stateChange(form, data),
        history: "replace",
        refreshList: true,
        keepReaderScroll: true,
      });
    }
  });

  addEventListener("popstate", () => void navigate(location.href, { history: "none" }));
  setCursor(articleRows().find(row => row.dataset.articleId === keyboardCursorID));
  try {
    const saved = JSON.parse(sessionStorage.getItem(pendingFolderKey));
    if (Array.isArray(saved)) {
      for (const [id, open] of saved) {
        if (typeof id !== "string" || !/^\d+$/.test(id) || typeof open !== "boolean") continue;
        const state = { open };
        pendingFolderStates.set(id, state);
        state.done = saveFolderState(id, state);
      }
    }
  } catch {
    // Use the database state when there are no recoverable local writes.
  }
  restoreFolders(document.querySelector(".sidebar"));
  scheduleContentCheck();
})();
