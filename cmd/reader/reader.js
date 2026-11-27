(() => {
  "use strict";

  let pending;
  let navigationGeneration = 0;
  let currentContext = contextKey(location.href);
  let mobileListScroll = 0;
  const mobileLayout = matchMedia("(max-width: 600px)");
  const sidebarToggle = document.querySelector(".sidebar-toggle");
  let desktopSidebarExpanded = true;
  let mobileSidebarExpanded = false;

  function updateSidebarToggle() {
    const expanded = mobileLayout.matches ? mobileSidebarExpanded : desktopSidebarExpanded;
    sidebarToggle.setAttribute("aria-expanded", String(expanded));
  }

  function setSidebarExpanded(expanded) {
    if (mobileLayout.matches) mobileSidebarExpanded = expanded;
    else desktopSidebarExpanded = expanded;
    updateSidebarToggle();
  }

  sidebarToggle.addEventListener("click", () => {
    const expanded = sidebarToggle.getAttribute("aria-expanded") !== "true";
    setSidebarExpanded(expanded);
    if (mobileLayout.matches) {
      const focusTarget = expanded ? document.getElementById("sidebar-controls") : sidebarToggle;
      focusTarget.focus({ preventScroll: true });
    }
  });
  mobileLayout.addEventListener("change", updateSidebarToggle);
  updateSidebarToggle();
  sidebarToggle.hidden = false;

  const bulkReadCursor = Symbol("bulkRead");
  let keyboardCursorID = selectedID();
  let pageMovePending = false;
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
    (document.querySelector(".subscription-manager[open]") || document.body).append(notice);
  }

  async function navigate(raw, options = {}) {
    const url = new URL(raw, location.href);
    if (url.origin !== location.origin) return;
    ++navigationGeneration;
    contentStatus.cancel();
    pending?.abort();
    const controller = new AbortController();
    pending = controller;
    document.querySelector(".workspace-error")?.remove();
    document.querySelector("#reader-pane")?.setAttribute("aria-busy", "true");

    try {
      await folders.waitForSaves();
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
        if (response.status === 409 && options.reloadOnConflict && !controller.signal.aborted) {
          if (await navigate(location.href, { history: "none", refreshList: true, keepReaderScroll: true })) {
            showError(message.trim());
          }
          return;
        }
        throw new Error(message.length < 300 ? message : "読み込めませんでした。再度お試しください。");
      }
      if (options.openURL) {
        const result = await response.json();
        if (controller.signal.aborted) return;
        return applyOpenedArticle(options.openURL, result);
      }

      const html = await response.text();
      if (controller.signal.aborted) return;
      const responseURL = new URL(response.url);
      const nextContext = contextKey(responseURL);
      const sameContext = nextContext === currentContext;
      const change = options.stateChange;
      if (sameContext && change?.kind === "favorite" && !isFavoritesView(responseURL.searchParams)) {
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

      const filter = responseURL.searchParams.get("view");
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
        oldList.querySelector("[data-result-count]").textContent = nextList.querySelector("[data-result-count]").textContent;
      } else if (refreshWorkspace) {
        oldList.replaceWith(nextList);
        pagination.observeNextPage();
        nextList.scrollTop = sameContext ? listScroll : 0;
      }
      if (refreshWorkspace) {
        const sidebar = document.querySelector(".sidebar");
        const sidebarScroll = sidebar.scrollTop;
        folders.restoreFolders(nextSidebar);
        const manager = sidebar.querySelector(".subscription-manager[open]");
        const managerScroll = manager?.scrollTop;
        const paneScroll = new Map(Array.from(manager?.querySelectorAll("[data-manager-pane]") || [],
          pane => [pane.dataset.managerPane, pane.scrollTop]));
        manager?.close();
        nextSidebar.querySelector(".sidebar-toggle").replaceWith(sidebarToggle);
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
      if (oldID !== nextID) contentStatus.reset();
      oldReader.replaceWith(nextReader);
      if (options.keepReaderScroll && oldID === nextID) nextReader.scrollTop = readerScroll;
      document.querySelector(".shell").classList.toggle("has-detail", Boolean(nextID));
      const returnTo = nextList.querySelector('input[name="return_to"]')?.value ||
        responseURL.pathname + responseURL.search;
      updateSelectedRows(nextID, returnTo);
      if (sameContext && (!options.refreshList || keepArticleList)) {
        document.querySelector("#list-pane .notice")?.remove();
        oldList.scrollTop = listScroll;
      }

      const rows = articleRows();
      if (!sameContext || nextID !== oldID) {
        setCursor(rows.find(row => row.dataset.articleId === nextID));
      } else if (keyboardCursorID === bulkReadCursor) {
        setCursor(document.querySelector("#list-pane [data-mark-loaded-read]"));
      } else {
        setCursor(
          rows.find(row => row.dataset.articleId === keyboardCursorID) ||
            (cursorIndex >= 0 ? rows[Math.min(cursorIndex, rows.length - 1)] : null),
        );
      }

      currentContext = nextContext;
      if (options.history === "replace") history.replaceState({}, "", response.url);
      else if (options.history !== "none") history.pushState({}, "", response.url);
      if (!options.keepReaderScroll && !document.querySelector("dialog[open]")) {
        if (mobileLayout.matches && mobileSidebarExpanded && (!sameContext || options.closeMobileNavigation)) {
          setSidebarExpanded(false);
        }
        (nextID ? nextReader : document.querySelector("#list-pane")).focus({ preventScroll: true });
        if (mobileLayout.matches) {
          window.scrollTo(0, !nextID && sameContext && !options.closeMobileNavigation ? mobileListScroll : 0);
        }
      }
      return true;
    } catch (error) {
      if (error.name !== "AbortError") showError(error.message || "読み込めませんでした。");
    } finally {
      if (pending === controller) {
        document.querySelector("#reader-pane")?.removeAttribute("aria-busy");
        contentStatus.scheduleContentCheck();
      }
    }
  }

  const contentStatus = (() => {
    let contentPollTimer;
    let displayRequest;

    function cancel() {
      clearTimeout(contentPollTimer);
    }

    function reset() {
      displayRequest = undefined;
    }

    function newViewID() {
      // getRandomValues also works over HTTP on a local network.
      const bytes = crypto.getRandomValues(new Uint8Array(16));
      bytes[6] = (bytes[6] & 0x0f) | 0x40;
      bytes[8] = (bytes[8] & 0x3f) | 0x80;
      const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
      return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join("-");
    }

    function scheduleContentCheck() {
      clearTimeout(contentPollTimer);
      const reader = document.querySelector(".reader[data-reading-id]");
      if (!reader) return;
      const id = reader.dataset.readingId;
      displayRequest ??= { id: newViewID(), complete: false };
      const request = displayRequest;
      const requestNow = !request.complete;
      const fetched = reader.dataset.contentFetched === "true";
      if (!requestNow && (fetched || reader.dataset.contentError === "true")) return;
      const hadError = reader.dataset.contentError === "true";
      const generation = navigationGeneration;

      async function checkContent() {
        try {
          const readStatus = async (method) => {
            const headers = { Accept: "application/json" };
            if (method === "POST") headers["X-Article-View-ID"] = request.id;
            const response = await fetch(`/articles/${id}/content-status`, {
              method, credentials: "same-origin", headers,
            });
            if (!response.ok) throw new Error();
            return response.json();
          };
          if (requestNow && !request.result) {
            request.result = readStatus("POST").then(status => {
              request.complete = true;
              return status;
            }, error => {
              request.result = undefined;
              throw error;
            });
          }
          const status = await (requestNow ? request.result : readStatus("GET"));
          if (generation !== navigationGeneration || selectedID() !== id) return;
          if ((status.fetched && !fetched) || Boolean(status.error) !== hadError) {
            await navigate(location.href, { history: "none", keepReaderScroll: true });
          } else if (status.pending) {
            scheduleContentCheck();
          }
        } catch {
          if (generation === navigationGeneration && selectedID() === id) {
            contentPollTimer = setTimeout(scheduleContentCheck, 5000);
          }
        }
      }

      if (requestNow) void checkContent();
      else contentPollTimer = setTimeout(checkContent, 5000);
    }

    return { cancel, reset, scheduleContentCheck };
  })();

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
    return {
      id: id?.articleId || id?.readingId,
      kind: data.get("kind"),
      value: data.get("value"),
    };
  }

  function articleRows() {
    return Array.from(document.querySelectorAll("#list-pane [data-article-id]"));
  }

  function updateBulkReadAvailability(button, rows) {
    if (!button) return;
    const hasUnread = rows.some(row => !row.classList.contains("read"));
    button.setAttribute("aria-disabled", String(!hasUnread));
  }

  async function markLoadedRead(button) {
    if (pageMovePending || favoritePending || button.getAttribute("aria-disabled") === "true") return;
    const ids = new Set(articleRows().filter(row => !row.classList.contains("read")).map(row => row.dataset.articleId));
    if (!ids.size) return;
    const status = button.parentElement.querySelector("[data-bulk-read-status]");
    const body = new URLSearchParams();
    for (const id of ids) body.append("id", id);
    button.setAttribute("aria-disabled", "true");
    pageMovePending = true;
    status.textContent = "既読にしています…";
    try {
      document.querySelector(".workspace-error")?.remove();
      const response = await fetch("/articles/mark-read", {
        method: "POST",
        body,
        credentials: "same-origin",
        headers: { Accept: "application/json" },
      });
      if (!response.ok) throw new Error();
      const { count } = await response.json();
      if (!Number.isSafeInteger(count) || count < 0) throw new Error();
      // A fragment-only change on the home page would skip the reload.
      const home = new URL("/", location.href);
      home.hash = `marked-read=${count}`;
      if (location.pathname === "/" && !location.search) {
        history.replaceState(history.state, "", home);
        location.reload();
      } else {
        location.assign(home);
      }
    } catch {
      showError("記事を既読にできませんでした。もう一度お試しください。");
    } finally {
      status.textContent = "";
      updateBulkReadAvailability(button, articleRows());
      pageMovePending = false;
    }
  }

  function showBulkReadToast() {
    if (location.pathname !== "/" || location.search) return;
    const match = /^#marked-read=(\d+)$/.exec(location.hash);
    if (!match) return;
    history.replaceState(history.state, "", "/");
    if (mobileLayout.matches) {
      setSidebarExpanded(true);
      document.getElementById("sidebar-controls").focus({ preventScroll: true });
    }
    const toast = document.querySelector("#bulk-read-toast");
    toast.textContent = `${match[1]}件の記事を既読にしました。`;
    setTimeout(() => { toast.textContent = ""; }, 5000);
  }

  function revealListItem(item) {
    item.scrollIntoView({ block: "nearest", inline: "nearest" });
    const pane = item.closest("#list-pane");
    const headerBottom = pane.querySelector(".list-header").getBoundingClientRect().bottom;
    const overlap = headerBottom - item.getBoundingClientRect().top;
    if (overlap <= 0) return;
    const scroller = mobileLayout.matches ? window : pane;
    scroller.scrollBy(0, -Math.ceil(overlap));
  }

  function scrollToCursor(rows, index) {
    const row = rows[index];
    const pane = row.closest("#list-pane");
    if (pane.clientHeight === 0) return;
    if (mobileLayout.matches) {
      revealListItem(row);
      return;
    }

    const paneTop = pane.getBoundingClientRect().top + pane.clientTop;
    const top = pane.querySelector(".list-header").getBoundingClientRect().bottom;
    const bottom = paneTop + pane.clientHeight;
    // Keep five titles on either side when the list pane has enough room.
    const start = Math.max(0, index - 5);
    const titles = rows.slice(start, index + 6).map(item => item.querySelector("h2").getBoundingClientRect());
    const current = index - start;

    let first = 0;
    let last = titles.length - 1;
    while (titles[last].bottom - titles[first].top > bottom - top && first < last) {
      if (current - first > last - current) first++;
      else last--;
    }

    if (titles[first].top < top) pane.scrollTop += Math.floor(titles[first].top - top);
    else if (titles[last].bottom > bottom) pane.scrollTop += Math.ceil(titles[last].bottom - bottom);
  }

  function setCursor(item, scroll = false) {
    const previous = keyboardCursorID;
    const bulkRead = document.querySelector("#list-pane [data-mark-loaded-read]");
    const onBulkRead = Boolean(bulkRead && item === bulkRead);
    keyboardCursorID = onBulkRead ? bulkReadCursor : item?.dataset.articleId || "";

    const rows = articleRows();
    for (const row of rows) row.classList.toggle("keyboard-cursor", row === item);
    updateBulkReadAvailability(bulkRead, rows);
    bulkRead?.classList.toggle("keyboard-cursor", onBulkRead);

    if (scroll && onBulkRead) {
      bulkRead.focus({ preventScroll: true });
      revealListItem(bulkRead);
    } else if (scroll && item) scrollToCursor(rows, rows.indexOf(item));

    const status = document.querySelector("#keyboard-status");
    if (status && previous !== keyboardCursorID) {
      status.textContent = onBulkRead ? bulkRead.textContent : item?.querySelector("h2")?.textContent || "";
    }
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
      if (document.querySelector('[data-unread-filter][aria-current="true"]')) {
        const count = document.querySelector("#list-pane [data-result-count]");
        count.textContent = Math.max(0, parseInt(count.textContent, 10) - 1) + "件";
      }
    }

    contentStatus.reset();
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

  const pagination = (() => {
    let nextPageRequest;
    const narrowWorkspace = matchMedia("(max-width: 600px)");
    let nextPageObserver;

    function observeNextPage() {
      nextPageObserver?.disconnect();
      const list = document.querySelector("#list-pane");
      const link = list.querySelector("[data-next-articles]");
      if (!link) return;
      nextPageObserver = new IntersectionObserver((entries, observer) => {
        if (observer !== nextPageObserver) return;
        for (const entry of entries) {
          if (entry.isIntersecting) void loadNextPage(entry.target);
        }
      }, {
        root: narrowWorkspace.matches ? null : list,
        rootMargin: "0px 0px 320px 0px",
      });
      nextPageObserver.observe(link);
    }

    narrowWorkspace.addEventListener("change", observeNextPage);

    async function appendNextPage(link) {
      const footer = link.closest(".auto-paging");
      link.setAttribute("aria-busy", "true");
      try {
        const response = await fetch(link.href, {
          credentials: "same-origin",
          headers: { Accept: "text/vnd.rss-reader.article-page+html" },
        });
        if (!response.ok) throw new Error();
        const html = await response.text();
        if (!link.isConnected) return;
        const page = new DOMParser().parseFromString(html, "text/html");

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
        updateBulkReadAvailability(document.querySelector("#list-pane [data-mark-loaded-read]"), articleRows());
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

    return { observeNextPage, loadNextPage };
  })();

  async function moveCursor(delta) {
    if (pageMovePending || favoritePending) return;
    pageMovePending = true;
    try {
      const list = document.querySelector("#list-pane");
      let rows = articleRows();
      const onBulkRead = keyboardCursorID === bulkReadCursor;
      if (onBulkRead && delta > 0) return;
      const index = rows.findIndex(row => row.dataset.articleId === keyboardCursorID);
      const next = onBulkRead ? rows.length - 1 : index < 0 ? 0 : index + delta;
      const nextPage = list.querySelector("[data-next-articles]");
      const preload = next >= 0 && rows.length - next <= 5
        ? pagination.loadNextPage(nextPage)
        : undefined;
      if (!rows[next] && delta > 0) {
        await preload;
        if (!list.isConnected) return;
        rows = articleRows();
      }
      const row = rows[next];
      if (row) {
        await openArticle(row, row.querySelector("a[data-workspace-link]").href);
      } else if (delta > 0 && !list.querySelector("[data-next-articles]")) {
        const bulkRead = list.querySelector("[data-mark-loaded-read]");
        if (!bulkRead) return;
        if (!list.getClientRects().length) {
          const close = document.querySelector(".reader-toolbar [data-workspace-link]");
          if (!close || !await navigate(close.href) || !list.isConnected) return;
        }
        setCursor(bulkRead, true);
      }
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
    if (keyboardCursorID === bulkReadCursor) return;
    document.querySelector(".reader [data-original-link]")?.click();
  }

  async function toggleCursorFavorite() {
    if (favoritePending || pageMovePending || keyboardCursorID === bulkReadCursor) return;
    const row = articleRows().find(item => item.dataset.articleId === keyboardCursorID);
    const scope = row || document.querySelector(".reader");
    const form = scope && Array.from(scope.querySelectorAll("form")).find(
      item => item.querySelector('input[name="kind"]')?.value === "favorite",
    );
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
    if (mobileLayout.matches && selectedID()) {
      const close = document.querySelector(".reader-toolbar [data-workspace-link]");
      if (close && !await navigate(close.href)) return;
    }
    setSidebarExpanded(true);
    document.querySelector(selector).focus();
  }

  document.addEventListener("keydown", event => {
    const target = event.target instanceof Element ? event.target : document.activeElement;
    if (event.defaultPrevented || event.isComposing || event.ctrlKey || event.metaKey || event.altKey) return;
    if (event.key === "Escape" && mobileLayout.matches &&
        sidebarToggle.getAttribute("aria-expanded") === "true" &&
        target?.closest(".sidebar") && !document.querySelector("dialog[open]")) {
      event.preventDefault();
      setSidebarExpanded(false);
      sidebarToggle.focus({ preventScroll: true });
      return;
    }
    if (target?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])') || document.querySelector("dialog[open]")) return;
    if (!event.shiftKey && event.key === "Enter" && keyboardCursorID === bulkReadCursor) {
      const button = document.querySelector("#list-pane [data-mark-loaded-read]");
      const control = target?.closest('button, summary, a, [role="button"]');
      if (!button?.getClientRects().length || (control && control !== button)) return;
      event.preventDefault();
      if (!event.repeat) void markLoadedRead(button);
    } else if (event.key === "/") {
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
    } else if (!event.shiftKey && event.key === "r") {
      event.preventDefault();
      if (!event.repeat) location.assign("/");
    }
  });

  const folders = (() => {
    const pendingFolderStates = new Map();
    let folderOrderPending = false;

    async function saveFolderOrder(previous, ids) {
      folderOrderPending = true;
      try {
        const body = new URLSearchParams({ return_to: location.pathname + location.search });
        for (const id of previous) body.append("previous_folder", id);
        for (const id of ids) body.append("folder", id);
        return await navigate("/folders/order", {
          method: "POST",
          body,
          history: "replace",
          refreshList: true,
          keepReaderScroll: true,
          reloadOnConflict: true,
        });
      } finally {
        folderOrderPending = false;
      }
    }

    function waitForSaves() {
      return Promise.all([...pendingFolderStates.values()].map(state => state.done));
    }

    async function moveOrder(orderButton) {
      if (folderOrderPending) return;
      const folder = orderButton.closest("[data-sortable-folder]");
      const folders = [...folder.parentElement.querySelectorAll(":scope > [data-sortable-folder]")];
      const from = folders.indexOf(folder);
      const to = from + Number(orderButton.dataset.folderStep);
      if (!folders[to]) return;
      const previous = folders.map(item => item.dataset.sortableFolder);
      [folders[from], folders[to]] = [folders[to], folders[from]];
      const saved = await saveFolderOrder(previous, folders.map(item => item.dataset.sortableFolder));
      if (saved) {
        const current = document.querySelector(
          `.subscription-manager[open] .folder-board > [data-sortable-folder="${folder.dataset.sortableFolder}"]`,
        );
        const button = current?.querySelector(`[data-folder-step="${orderButton.dataset.folderStep}"]:not(:disabled)`);
        (button || current?.querySelector("[data-folder-step]:not(:disabled)"))?.focus();
      }
    }

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

    return { restoreFolders, waitForSaves, moveOrder };
  })();

  document.addEventListener("close", event => {
    if (event.target.matches?.(".subscription-manager") && event.target.isConnected) {
      event.target.closest(".sidebar").querySelector("[data-manage-feeds]")?.focus();
    }
  }, true);

  document.addEventListener("click", async (event) => {
    if (event.target.closest("[data-close-sidebar]")) {
      setSidebarExpanded(false);
      sidebarToggle.focus({ preventScroll: true });
      return;
    }
    const markRead = event.target.closest("[data-mark-loaded-read]");
    if (markRead) {
      await markLoadedRead(markRead);
      return;
    }
    const nextPage = event.target.closest("[data-next-articles]");
    if (nextPage && event.button === 0 && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey) {
      event.preventDefault();
      await pagination.loadNextPage(nextPage);
      return;
    }
    if (event.target.closest("[data-show-shortcuts]")) {
      document.querySelector("#shortcuts-dialog").showModal();
      return;
    }
    const orderButton = event.target.closest("[data-folder-step]");
    if (orderButton) {
      await folders.moveOrder(orderButton);
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
      await navigate(link.href, {
        refreshList: link.hasAttribute("data-history-link"),
        closeMobileNavigation: Boolean(link.closest(".sidebar")),
      });
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

  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (!form.closest("#workspace, .sidebar")) return;
    if (form.hasAttribute("data-workspace-search")) {
      event.preventDefault();
      const url = new URL(form.action);
      url.search = new URLSearchParams(new FormData(form)).toString();
      void navigate(url, { closeMobileNavigation: true });
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
  folders.restoreFolders(document.querySelector(".sidebar"));
  pagination.observeNextPage();
  contentStatus.scheduleContentCheck();
  showBulkReadToast();
})();
