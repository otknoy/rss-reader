(() => {
  "use strict";

  let pending;
  let contentPollTimer;
  let navigationGeneration = 0;
  let currentContext = contextKey(location.href);
  let mobileListScroll = 0;

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
    if (!reader) return;
    const id = reader.dataset.readingId;
    const hadError = reader.dataset.contentError === "true";
    const generation = navigationGeneration;
    contentPollTimer = setTimeout(async () => {
      try {
        const response = await fetch(`/articles/${id}/content-status`, {
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
    }, hadError ? 30000 : 5000);
  }

  document.addEventListener("click", (event) => {
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
  scheduleContentCheck();
})();
