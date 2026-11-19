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
  let favoritePending = false;
  let nextPageRequest;

  const pendingFolderStates = new Map();
  let folderOrderPending = false;

  async function saveFolderOrder(ids) {
    folderOrderPending = true;
    try {
      const body = new URLSearchParams({ return_to: location.pathname + location.search });
      for (const id of ids) body.append("folder", id);
      return await navigate("/folders/order", {
        method: "POST", body, history: "replace", refreshList: true, keepReaderScroll: true,
      });
    } finally {
      folderOrderPending = false;
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
        headers: { Accept: options.openURL ? "application/json" : "text/html" },
      });
      if (!response.ok) {
        const message = await response.text();
        throw new Error(message.length < 300 ? message : "読み込めませんでした。再度お試しください。");
      }
      if (options.openURL) {
        const result = await response.json();
        if (controller.signal.aborted) return;
        return applyOpenedArticle(options.openURL, result);
      }
      const html = await response.text();
      if (controller.signal.aborted) return;
      const nextContext = contextKey(response.url);
      const sameContext = nextContext === currentContext;
      const change = options.stateChange;
      if (sameContext && change?.kind === "favorite" && !isFavoritesView(new URL(response.url).searchParams)) {
        // The result set is unchanged. Keep the rows, keyboard cursor and scroll in place.
        for (const article of document.querySelectorAll("[data-article-id], .reader[data-reading-id]")) {
          if ((article.dataset.articleId || article.dataset.readingId) === change.id) {
            updateFavoriteButton(article, change.value === "true");
          }
        }
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

      const filter = new URL(response.url).searchParams.get("view");
      const keepArticleList = sameContext && filter !== "read" &&
        (change?.kind === "read" || change?.kind === "favorite");
      const refreshWorkspace = !sameContext || options.refreshList;
      if (keepArticleList) {
        const row = articleRows().find(item => item.dataset.articleId === change.id);
        if (row && change.kind === "read") updateReadRow(row, change.value === "true");
        if (row && change.kind === "favorite") {
          if (change.value === "true") updateFavoriteButton(row, true);
          else row.remove();
        }
        oldList.querySelector(".results .muted").textContent = nextList.querySelector(".results .muted").textContent;
      } else if (refreshWorkspace) {
        oldList.replaceWith(nextList);
        observeNextPage();
        nextList.scrollTop = sameContext ? listScroll : 0;
      }
      if (refreshWorkspace) {
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
      updateSelectedRows(nextID, nextList.querySelector('input[name="return_to"]')?.value || response.url);
      if (sameContext && (!options.refreshList || keepArticleList)) {
        document.querySelector("#list-pane .notice")?.remove();
        oldList.scrollTop = listScroll;
      }

      const rows = articleRows();
      if (!sameContext || nextID !== oldID) {
        setCursor(rows.find(row => row.dataset.articleId === nextID));
      } else {
        setCursor(rows.find(row => row.dataset.articleId === keyboardCursorID) || (cursorIndex >= 0 ? rows[Math.min(cursorIndex, rows.length - 1)] : null));
      }

      currentContext = nextContext;
      if (options.history === "replace") history.replaceState({}, "", response.url);
      else if (options.history !== "none") history.pushState({}, "", response.url);
      if (!options.keepReaderScroll && !document.querySelector("dialog[open]")) {
        (nextID ? nextReader : document.querySelector("#list-pane")).focus({ preventScroll: true });
        if (matchMedia("(max-width: 600px)").matches) window.scrollTo(0, nextID ? 0 : mobileListScroll);
      }
      return true;
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

  function isFavoritesView(params) {
    const view = params.get("view");
    return view === "favorites" || view === "unread-favorites";
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

  function updateReadRow(row, read) {
    row.classList.toggle("read", read);
    const form = row.querySelector('input[name="kind"][value="read"]').form;
    form.elements.namedItem("value").value = String(!read);
    form.querySelector("button").textContent = read ? "未読に戻す" : "既読にする";
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
    const start = Math.max(0, index - 5);
    const titles = rows.slice(start, index + 6).map(item => item.querySelector("h2").getBoundingClientRect());
    const current = index - start;
    let first = 0;
    let last = titles.length - 1;
    while (titles[last].bottom - titles[first].top > pane.clientHeight && first < last) {
      if (current - first > last - current) first++;
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

  function updateSelectedRows(id, returnTo) {
    for (const row of articleRows()) {
      const selected = row.dataset.articleId === id;
      row.classList.toggle("selected", selected);
      const link = row.querySelector("h2 a");
      if (selected) link.setAttribute("aria-current", "true");
      else link.removeAttribute("aria-current");
      for (const input of row.querySelectorAll('input[name="return_to"]')) input.value = returnTo;
    }
  }

  function decrementUnreadBadge(link) {
    const count = link?.querySelector("[data-unread-count]");
    if (!count) return;
    const remaining = Number(count.textContent) - 1;
    if (remaining > 0) count.textContent = remaining;
    else count.closest(".unread-badge").remove();
  }

  function applyOpenedArticle(url, result) {
    const next = new DOMParser().parseFromString(result.html, "text/html");
    const reader = next.querySelector("#reader-pane");
    const id = selectedID(next);
    const row = articleRows().find(item => item.dataset.articleId === id);
    if (!reader || !row || id !== url.searchParams.get("article")) {
      throw new Error("記事を開けませんでした。");
    }

    if (!selectedID()) mobileListScroll = window.scrollY;
    updateReadRow(row, true);
    if (result.changed) {
      const feed = document.querySelector(`.feed-list a[data-feed-drag="${CSS.escape(result.sourceId)}"]`);
      decrementUnreadBadge(feed);
      decrementUnreadBadge(feed?.closest("[data-sidebar-folder]")?.querySelector("summary > a"));
      if (["unread", "unread-favorites"].includes(url.searchParams.get("view"))) {
        const count = document.querySelector("#list-pane .results .muted");
        count.textContent = Math.max(0, parseInt(count.textContent, 10) - 1) + "件";
      }
    }

    document.querySelector("#reader-pane").replaceWith(reader);
    document.querySelector(".shell").classList.add("has-detail");
    updateSelectedRows(id, url.pathname + url.search);
    setCursor(row, true);
    history.pushState({}, "", url);
    if (!document.querySelector("dialog[open]")) {
      reader.focus({ preventScroll: true });
      if (matchMedia("(max-width: 600px)").matches) window.scrollTo(0, 0);
    }
    return true;
  }

  async function openArticle(row, href) {
    const url = new URL(href);
    await navigate(`/articles/${row.dataset.articleId}/open`, {
      method: "POST",
      body: new URLSearchParams({ return_to: url.pathname + url.search }),
      openURL: url,
    });
  }

  const nextPageObserver = new IntersectionObserver(entries => {
    for (const entry of entries) {
      if (entry.isIntersecting) void loadNextPage(entry.target);
    }
  });

  function observeNextPage() {
    nextPageObserver.disconnect();
    const link = document.querySelector("[data-next-articles]");
    if (link) nextPageObserver.observe(link);
  }

  async function appendNextPage(link) {
    const footer = link.closest(".auto-paging");
    link.setAttribute("aria-busy", "true");
    try {
      const response = await fetch(link.href, { credentials: "same-origin" });
      if (!response.ok) throw new Error();
      const html = await response.text();
      if (!link.isConnected) return;
      const page = new DOMParser().parseFromString(html, "text/html").querySelector("#list-pane");
      if (!page) throw new Error();

      const existing = new Set(articleRows().map(row => row.dataset.articleId));
      for (const row of page.querySelectorAll("[data-article-id]")) {
        if (existing.has(row.dataset.articleId)) continue;
        const articleLink = row.querySelector("h2 a");
        const articleURL = new URL(location.href);
        articleURL.searchParams.set("article", row.dataset.articleId);
        articleLink.href = articleURL.href;
        for (const input of row.querySelectorAll('input[name="return_to"]')) {
          input.value = location.pathname + location.search;
        }
        footer.before(row);
      }
      const next = page.querySelector(".auto-paging");
      const nextHref = next?.querySelector("a").getAttribute("href");
      if (nextHref && new URL(nextHref, response.url).href !== link.href) footer.replaceWith(next);
      else footer.remove();
      observeNextPage();
    } catch {
      if (link.isConnected) {
        nextPageObserver.unobserve(link);
        showError("次の記事を読み込めませんでした。もう一度お試しください。");
      }
    } finally {
      link.removeAttribute("aria-busy");
    }
  }

  function loadNextPage(link) {
    if (!link?.isConnected) return;
    if (nextPageRequest?.link === link) return nextPageRequest.done;
    const done = appendNextPage(link);
    nextPageRequest = { link, done };
    void done.finally(() => {
      if (nextPageRequest?.link === link) nextPageRequest = undefined;
    });
    return done;
  }

  async function moveCursor(delta) {
    if (pageMovePending || favoritePending) return;
    pageMovePending = true;
    try {
      let rows = articleRows();
      const index = rows.findIndex(row => row.dataset.articleId === keyboardCursorID);
      const next = index < 0 ? 0 : index + delta;
      if (!rows[next] && delta > 0) {
        const list = document.querySelector("#list-pane");
        await loadNextPage(list.querySelector("[data-next-articles]"));
        if (!list.isConnected) return;
        rows = articleRows();
      }
      const row = rows[next];
      if (!row) return;
      await openArticle(row, row.querySelector("a[data-workspace-link]").href);
    } finally {
      pageMovePending = false;
    }
  }

  async function moveSidebarSelection(delta) {
    if (pageMovePending || favoritePending) return;
    const sidebar = document.querySelector(".sidebar");
    const links = [...sidebar.querySelectorAll("[data-sidebar-scope]")];
    const index = links.findIndex(link => link.hasAttribute("aria-current"));
    const next = index < 0 ? 0 : index + delta;
    if (!links[next]) return;

    pageMovePending = true;
    try {
      if (await navigate(links[next].href) && !matchMedia("(max-width: 600px)").matches) {
        document.querySelector(".sidebar [data-sidebar-scope][aria-current]")
          ?.scrollIntoView({ block: "nearest", inline: "nearest" });
      }
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

  async function focusSearch() {
    const selector = '[data-workspace-search] input[type="search"]';
    const search = document.querySelector(selector);
    if (!search.getClientRects().length) {
      const close = document.querySelector(".reader-toolbar [data-workspace-link]");
      if (close) await navigate(close.href);
    }
    document.querySelector(selector).focus();
  }

  document.addEventListener("keydown", event => {
    const target = event.target instanceof Element ? event.target : document.activeElement;
    if (event.defaultPrevented || event.isComposing || event.ctrlKey || event.metaKey || event.altKey) return;
    if (target?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])') || document.querySelector("dialog[open]")) return;
    if (event.key === "/") {
      event.preventDefault();
      if (!event.repeat) void focusSearch();
    } else if (event.key === "?") {
      event.preventDefault();
      if (!event.repeat) document.querySelector("#shortcuts-dialog").showModal();
    } else if (event.key === " " && selectedID()) {
      if (target?.closest('button, summary, a, [role="button"]')) return;
      const pane = document.querySelector("#reader-pane");
      if (!pane || getComputedStyle(pane).display === "none") return;
      event.preventDefault();
      pane.scrollBy({
        top: (event.shiftKey ? -1 : 1) * Math.max(80, pane.clientHeight * .8),
        behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
      });
    } else if (event.key.toLowerCase() === "j" || event.key.toLowerCase() === "k") {
      event.preventDefault();
      const delta = event.key.toLowerCase() === "j" ? 1 : -1;
      if (event.shiftKey) void moveSidebarSelection(delta);
      else void moveCursor(delta);
    } else if (!event.shiftKey && event.key === "i") {
      event.preventDefault();
      if (!event.repeat) openOriginal();
    } else if (!event.shiftKey && event.key === "f") {
      event.preventDefault();
      if (!event.repeat) void toggleCursorFavorite();
    }
  });

  function restoreFolders(sidebar) {
    const folders = sidebar.querySelectorAll(".folder-board > [data-sortable-folder]");
    folders.forEach((folder, index) => {
      for (const button of folder.querySelectorAll("[data-folder-step]")) {
        button.disabled = button.dataset.folderStep === "-1" ? index === 0 : index === folders.length - 1;
      }
    });
    for (const folder of sidebar.querySelectorAll("[data-sidebar-folder]")) {
      const pendingState = pendingFolderStates.get(folder.dataset.sidebarFolder);
      if (pendingState) folder.open = pendingState.open;
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
    const nextPage = event.target.closest("[data-next-articles]");
    if (nextPage && event.button === 0 && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey) {
      event.preventDefault();
      await loadNextPage(nextPage);
      return;
    }
    if (event.target.closest("[data-show-shortcuts]")) {
      document.querySelector("#shortcuts-dialog").showModal();
      return;
    }
    const orderButton = event.target.closest("[data-folder-step]");
    if (orderButton) {
      if (folderOrderPending) return;
      const folder = orderButton.closest("[data-sortable-folder]");
      const folders = [...folder.parentElement.querySelectorAll(":scope > [data-sortable-folder]")];
      const from = folders.indexOf(folder);
      const to = from + Number(orderButton.dataset.folderStep);
      if (!folders[to]) return;
      [folders[from], folders[to]] = [folders[to], folders[from]];
      const saved = await saveFolderOrder(folders.map(item => item.dataset.sortableFolder));
      if (saved) {
        const current = document.querySelector(`.subscription-manager[open] .folder-board > [data-sortable-folder="${folder.dataset.sortableFolder}"]`);
        const button = current?.querySelector(`[data-folder-step="${orderButton.dataset.folderStep}"]:not(:disabled)`);
        (button || current?.querySelector("[data-folder-step]:not(:disabled)"))?.focus();
      }
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
  restoreFolders(document.querySelector(".sidebar"));
  observeNextPage();
  scheduleContentCheck();
})();
