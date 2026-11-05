(() => {
  var formDraftsMap = new Map();
  var draftSaveTimers = new Map();

  // Theme management
  function isDarkActive() {
    const attr = document.documentElement.getAttribute('data-theme');
    if (attr === 'dark') return true;
    if (attr === 'light') return false;
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  }

  function updateThemeToggleBtn() {
    const btn = document.getElementById('themeToggleBtn');
    if (!btn) return;
    const dark = isDarkActive();
    btn.textContent = dark ? '☀' : '☾';
    btn.title = dark ? 'Switch to Light Mode' : 'Switch to Dark Mode';
  }

  window.toggleTheme = function() {
    const next = isDarkActive() ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try {
      localStorage.setItem('review_theme', next);
    } catch (e) {}
    updateThemeToggleBtn();

    fetch('/api/prefs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: 'theme', val: next })
    }).catch(() => {});
  };

  // Syntax highlighting management
  function isSyntaxOff() {
    return document.documentElement.getAttribute('data-syntax') === 'off';
  }

  function updateSyntaxToggleBtn() {
    const btn = document.getElementById('syntaxToggleBtn');
    if (!btn) return;
    const off = isSyntaxOff();
    btn.textContent = off ? 'Syntax: Off' : 'Syntax: On';
    btn.title = off ? 'Enable Syntax Highlighting' : 'Disable Syntax Highlighting';
  }

  window.toggleSyntax = function() {
    const off = isSyntaxOff();
    const next = off ? 'on' : 'off';
    if (next === 'off') {
      document.documentElement.setAttribute('data-syntax', 'off');
    } else {
      document.documentElement.removeAttribute('data-syntax');
    }
    try {
      localStorage.setItem('review_syntax', next);
    } catch (e) {}
    updateSyntaxToggleBtn();

    fetch('/api/prefs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: 'syntax', val: next })
    }).catch(() => {});
  };

  function renderCountsBadgesHTML(counts) {
    if (!counts) return '';
    let html = '';
    if (counts.unresolved > 0) {
      html += `<span class="badge badge-count-unresolved">${counts.unresolved} unresolved</span>`;
    }
    if (counts.resolved > 0) {
      html += `<span class="badge badge-count-resolved">${counts.resolved} resolved</span>`;
    }
    if (counts.draft > 0) {
      html += `<span class="badge badge-draft">${counts.draft} draft</span>`;
    }
    return html;
  }

  function escapeAttrVal(s) {
    return String(s || '').replace(/["\\]/g, '\\$&');
  }

  function updateCommentCountBadges(filePath, fileCounts, changeCounts) {
    if (changeCounts) {
      const el = document.getElementById('changeCommentCounts');
      if (el) {
        el.innerHTML = renderCountsBadgesHTML(changeCounts);
        if (typeof enhanceChangeUnresolvedBadge === 'function') {
          enhanceChangeUnresolvedBadge();
        }
      }

      const relInfo = document.querySelector('.relation-chain-card li.current-change-row .commit-info');
      if (relInfo) {
        let badge = relInfo.querySelector('.badge-count-unresolved');
        if (changeCounts.unresolved > 0) {
          if (!badge) {
            badge = document.createElement('span');
            badge.className = 'badge badge-count-unresolved';
            badge.style.cssText = 'font-size: 10px; padding: 1px 4px;';
            relInfo.appendChild(badge);
          }
          badge.textContent = String(changeCounts.unresolved);
        } else if (badge) {
          badge.remove();
        }
      }
    }
    if (filePath && fileCounts) {
      const cleanPath = filePath.replace(/^\/+/, '');
      document.querySelectorAll(`span.comment-counts[data-file-counts="${escapeAttrVal(cleanPath)}"]`).forEach(el => {
        el.innerHTML = renderCountsBadgesHTML(fileCounts);
      });
    }
  }

  function saveExpandedFiles() {
    const allFilesEl = document.querySelector('.diff-all-files');
    if (!allFilesEl) return;
    const repoSlug = allFilesEl.getAttribute('data-repo') || '';
    const changeKey = allFilesEl.getAttribute('data-change') || '';
    if (!repoSlug || !changeKey) return;
    const storageKey = `review_expanded:${repoSlug}:${changeKey}`;
    const expanded = [];
    document.querySelectorAll('.diff-file-section').forEach(sec => {
      const body = sec.querySelector('.diff-file-body');
      if (body && body.style.display !== 'none') {
        const path = sec.getAttribute('data-file-path');
        if (path) expanded.push(path);
      }
    });
    try {
      sessionStorage.setItem(storageKey, JSON.stringify(expanded));
    } catch (e) {}
  }

  function ensureFileBodyLoaded(body) {
    if (!body) return Promise.resolve();
    const url = body.getAttribute('data-lazy-url');
    if (!url) return Promise.resolve();
    if (body._loadPromise) return body._loadPromise;
    body.innerHTML = '<div style="padding: 12px; color: var(--muted); font-size: 12px;">Loading diff…</div>';
    body._loadPromise = fetch(url, { cache: 'no-store' }).then(res => {
      if (!res.ok) throw new Error('failed');
      return res.text();
    }).then(html => {
      const beforeHeight = body.offsetHeight || 0;
      const sec = body.closest ? body.closest('.diff-file-section') : null;
      const secRect = (sec && sec.getBoundingClientRect) ? sec.getBoundingClientRect() : null;
      const headerEl = document.querySelector('.app-header');
      const stickyBottom = (headerEl && headerEl.getBoundingClientRect) ? (headerEl.getBoundingClientRect().bottom || 0) : 0;

      body.innerHTML = html;
      body.removeAttribute('data-lazy-url');
      body._loadPromise = null;

      const table = body.querySelector ? body.querySelector('.diff-table') : null;
      if (table) table._diffRows = null;

      if (secRect && secRect.bottom <= stickyBottom && typeof window !== 'undefined' && window.scrollY > 0) {
        const deltaHeight = (body.offsetHeight || 0) - beforeHeight;
        if (deltaHeight > 0 && typeof window.scrollBy === 'function') {
          window.scrollBy(0, deltaHeight);
        }
      }

      if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
      if (typeof scheduleStickyScopeUpdate === 'function') scheduleStickyScopeUpdate();
      if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
      if (typeof restoreSavedDrafts === 'function') restoreSavedDrafts(body);
    }).catch(() => {
      body._loadPromise = null;
      body.innerHTML = '<div style="padding: 12px; color: var(--danger, #cf222e); font-size: 12px;">Failed to load diff. <a href="#" onclick="ensureFileBodyLoaded(this.closest(\'.diff-file-body\')); return false;">Retry</a></div>';
    });
    return body._loadPromise;
  }
  window.ensureFileBodyLoaded = ensureFileBodyLoaded;

  function restoreExpandedFiles() {
    const allFilesEl = document.querySelector('.diff-all-files');
    if (!allFilesEl) return;
    const repoSlug = allFilesEl.getAttribute('data-repo') || '';
    const changeKey = allFilesEl.getAttribute('data-change') || '';
    if (!repoSlug || !changeKey) return;
    const storageKey = `review_expanded:${repoSlug}:${changeKey}`;
    try {
      const raw = sessionStorage.getItem(storageKey);
      if (!raw) return;
      const expanded = JSON.parse(raw);
      if (!Array.isArray(expanded)) return;
      const set = new Set(expanded);
      document.querySelectorAll('.diff-file-section').forEach(sec => {
        const path = sec.getAttribute('data-file-path');
        const body = sec.querySelector('.diff-file-body');
        const chevron = sec.querySelector('.file-collapse-btn');
        if (set.has(path)) {
          if (body) {
            body.style.display = 'block';
            ensureFileBodyLoaded(body);
          }
          sec.classList.remove('collapsed');
          if (chevron) chevron.textContent = '▾';
        } else {
          if (body) body.style.display = 'none';
          sec.classList.add('collapsed');
          if (chevron) chevron.textContent = '▸';
        }
      });
      if (typeof window.updateToggleAllFilesBtn === 'function') {
        window.updateToggleAllFilesBtn();
      }
    } catch (e) {}
  }

  function escapeHtml(str) {
    if (!str) return '';
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function flashThreadCard(card) {
    if (!card || !card.classList) return;
    if (card._flashTimer) {
      clearTimeout(card._flashTimer);
      card._flashTimer = null;
    }
    card.classList.remove('jump-target-flash');
    void card.offsetWidth;
    card.classList.add('jump-target-flash');
    card._flashTimer = setTimeout(() => {
      card.classList.remove('jump-target-flash');
      card._flashTimer = null;
      if (typeof cleanupCommentZIndicesIfNeeded === 'function') {
        cleanupCommentZIndicesIfNeeded();
      }
    }, 3000);
  }
  window.flashThreadCard = flashThreadCard;

  function prevElemSibling(el) {
    if (!el) return null;
    if (el.previousElementSibling !== undefined) return el.previousElementSibling;
    const kids = el.parentNode && el.parentNode.childNodes;
    if (!kids) return null;
    const idx = kids.indexOf ? kids.indexOf(el) : Array.prototype.indexOf.call(kids, el);
    for (let i = idx - 1; i >= 0; i--) {
      if (kids[i] && kids[i].tagName) return kids[i];
    }
    return null;
  }

  function nextElemSibling(el) {
    if (!el) return null;
    if (el.nextElementSibling !== undefined) return el.nextElementSibling;
    const kids = el.parentNode && el.parentNode.childNodes;
    if (!kids) return null;
    const idx = kids.indexOf ? kids.indexOf(el) : Array.prototype.indexOf.call(kids, el);
    if (idx < 0) return null;
    for (let i = idx + 1; i < kids.length; i++) {
      if (kids[i] && kids[i].tagName) return kids[i];
    }
    return null;
  }

  function rowChildElems(row) {
    if (!row) return [];
    if (row.children) return row.children;
    if (!row.childNodes) return [];
    const out = [];
    for (let i = 0; i < row.childNodes.length; i++) {
      if (row.childNodes[i] && row.childNodes[i].tagName) out.push(row.childNodes[i]);
    }
    return out;
  }

  function moveHeaderMatchesSide(headerRow, side) {
    if (!headerRow || !headerRow.classList || !headerRow.classList.contains('move-header-row')) {
      return false;
    }
    const cells = rowChildElems(headerRow);
    const hasHdr = (td) => !!(td && td.classList && td.classList.contains('move-header-cell'));
    if (cells.length === 4) {
      if (side === 'L') return hasHdr(cells[1]);
      if (side === 'R') return hasHdr(cells[3]);
      return hasHdr(cells[1]) || hasHdr(cells[3]);
    }
    return cells.length > 0 && hasHdr(cells[cells.length - 1]);
  }

  function flashDiffRows(rows, side) {
    if (!rows) return;
    const isNodeList = (typeof NodeList !== 'undefined' && rows instanceof NodeList) || (typeof HTMLCollection !== 'undefined' && rows instanceof HTMLCollection) || (rows && typeof rows.length === 'number' && typeof rows.item === 'function');
    const rawList = Array.isArray(rows) ? rows : (isNodeList ? Array.from(rows) : [rows]);
    if (rawList.length === 0) return;

    const list = [];
    const prevHdr = prevElemSibling(rawList[0]);
    if (moveHeaderMatchesSide(prevHdr, side)) {
      list.push(prevHdr);
    }
    for (let i = 0; i < rawList.length; i++) {
      list.push(rawList[i]);
      if (i + 1 < rawList.length) {
        const between = [];
        let cur = nextElemSibling(rawList[i]);
        let steps = 0;
        while (cur && cur !== rawList[i + 1] && steps < 8) {
          if (cur.classList && cur.classList.contains('move-header-row')) {
            between.push(cur);
          }
          cur = nextElemSibling(cur);
          steps++;
        }
        if (cur === rawList[i + 1] && between.length > 0) {
          for (let j = 0; j < between.length; j++) list.push(between[j]);
        }
      }
    }

    let cls = 'jump-target-flash';
    if (side === 'L') {
      cls = 'jump-target-flash-l';
    } else if (side === 'R') {
      cls = 'jump-target-flash-r';
    }

    for (let i = 0; i < list.length; i++) {
      const row = list[i];
      if (!row || !row.classList) continue;
      if (row._flashTimer) {
        clearTimeout(row._flashTimer);
        row._flashTimer = null;
      }
      row.classList.remove('jump-target-flash', 'jump-target-flash-l', 'jump-target-flash-r');
    }

    for (let i = 0; i < list.length; i++) {
      const row = list[i];
      if (row && row.offsetWidth !== undefined) {
        void row.offsetWidth;
        break;
      }
    }

    for (let i = 0; i < list.length; i++) {
      const row = list[i];
      if (!row || !row.classList) continue;
      row.classList.add(cls);
      row._flashTimer = setTimeout(() => {
        row.classList.remove(cls, 'jump-target-flash', 'jump-target-flash-l', 'jump-target-flash-r');
        row._flashTimer = null;
      }, 2600);
    }
  }
  window.flashDiffRows = flashDiffRows;

  async function scrollToHashLine() {
    const hash = window.location.hash;
    if (!hash) return;
    const threadMatch = hash.match(/^#thread-(\d+)(?:-msg-(\d+))?$/);
    if (threadMatch) {
      const threadId = threadMatch[1];
      const messageId = threadMatch[2] || '';
      let card = document.getElementById('thread-' + threadId);
      let expandedAny = false;
      if (!card) {
        const allSecs = document.querySelectorAll('.diff-file-section');
        for (const sec of allSecs) {
          const body = sec.querySelector ? sec.querySelector('.diff-file-body') : null;
          if (!body || !body.getAttribute || !body.getAttribute('data-lazy-url') || (body.hasAttribute && body.hasAttribute('data-loaded'))) {
            continue;
          }
          const countsEl = sec.querySelector ? sec.querySelector('.comment-counts') : null;
          if (countsEl && countsEl.querySelector && !countsEl.querySelector('.badge') && !sec.querySelector('.comment-counts .badge')) {
            continue;
          }
          if (window.ensureFileBodyLoaded) {
            await window.ensureFileBodyLoaded(body);
            card = document.getElementById('thread-' + threadId);
            if (card) break;
          }
        }
      }
      if (card) {
        const sec = card.closest ? card.closest('.diff-file-section') : null;
        if (sec && sec.classList.contains('collapsed')) {
          const body = sec.querySelector ? sec.querySelector('.diff-file-body') : null;
          const btn = sec.querySelector ? sec.querySelector('.file-collapse-btn') : null;
          sec.classList.remove('collapsed');
          if (body) body.style.display = 'block';
          if (btn) btn.textContent = '▾';
          expandedAny = true;
        }
        if (revealThreadForJump(card, messageId, { batch: true })) {
          expandedAny = true;
        }
        const candidateMsgEl = messageId ? document.getElementById('msg-' + messageId) : null;
        const msgEl = (candidateMsgEl && (!card.contains || card.contains(candidateMsgEl))) ? candidateMsgEl : null;
        const scrollTarget = msgEl || card;
        if (scrollTarget && scrollTarget.scrollIntoView) {
          scrollTarget.scrollIntoView({ block: 'center' });
        }
        bringCommentToFront(card);
        if (!card.classList.contains('new-comment-card')) {
          threadNavState.el = card;
          threadNavState.scrollY = window.scrollY || 0;
        }
        flashThreadCard(card);
      }
      if (expandedAny) {
        if (typeof handleNaturalPageHeightChange === 'function') handleNaturalPageHeightChange();
        if (typeof window.updateToggleAllFilesBtn === 'function') window.updateToggleAllFilesBtn();
        saveExpandedFiles();
        if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
        scheduleStickyScopeUpdate();
      }
      return;
    }
    const m = hash.match(/^#L(\d+)$/);
    if (!m) return;
    const lineNum = parseInt(m[1], 10);
    const sp = new URLSearchParams(window.location.search);
    const jumpSide = sp.get('side');
    const endParam = parseInt(sp.get('end') || '0', 10);
    const maxLine = Math.max(lineNum, endParam || lineNum);

    let el = null;
    if (jumpSide === 'L') {
      el = document.querySelector(`tr[data-line-l="${lineNum}"]`) ||
        document.getElementById(hash.slice(1)) ||
        document.querySelector(`tr[data-line-r="${lineNum}"]`);
    } else {
      el = document.getElementById(hash.slice(1)) ||
        document.querySelector(`tr[data-line-r="${lineNum}"]`) ||
        document.querySelector(`tr[data-line-l="${lineNum}"]`);
    }

    if (el) {
      const sec = el.closest('.diff-file-section');
      if (sec && sec.classList.contains('collapsed')) {
        const body = sec.querySelector('.diff-file-body');
        const btn = sec.querySelector('.file-collapse-btn');
        sec.classList.remove('collapsed');
        if (body) body.style.display = 'block';
        if (btn) btn.textContent = '▾';
        if (window.ensureFileBodyLoaded && body) {
          await window.ensureFileBodyLoaded(body);
        }
      }
    }

    // Expand any skip rows overlapping [lineNum, maxLine]
    const allDiffContainers = document.querySelectorAll ? document.querySelectorAll('.diff-container') : [];
    const skipScope = el
      ? ((el.closest && (el.closest('.diff-container') || el.closest('.diff-file-section'))) || document)
      : (allDiffContainers.length === 1 ? allDiffContainers[0] : document);
    const skipRows = skipScope && skipScope.querySelectorAll ? skipScope.querySelectorAll('tr.row-skip') : [];
    for (const skip of skipRows) {
      if (el && el.closest && skip.closest) {
        const activeCont = el.closest('.diff-container');
        const skipCont = skip.closest('.diff-container');
        if (activeCont && skipCont && activeCont !== skipCont) {
          continue;
        }
      }
      const lStart = parseInt(skip.getAttribute('data-left-start') || '0', 10);
      const lEnd = parseInt(skip.getAttribute('data-left-end') || '0', 10);
      const rStart = parseInt(skip.getAttribute('data-right-start') || '0', 10);
      const rEnd = parseInt(skip.getAttribute('data-right-end') || '0', 10);
      const sStart = jumpSide === 'L' ? lStart : (jumpSide === 'R' ? rStart : Math.max(lStart, rStart));
      const sEnd = jumpSide === 'L' ? lEnd : (jumpSide === 'R' ? rEnd : Math.max(lEnd, rEnd));
      if (sStart > 0 && Math.max(lineNum, sStart) <= Math.min(maxLine, sEnd)) {
        const sec = skip.closest ? skip.closest('.diff-file-section') : null;
        if (sec && sec.classList.contains('collapsed')) {
          const body = sec.querySelector('.diff-file-body');
          const btn = sec.querySelector('.file-collapse-btn');
          sec.classList.remove('collapsed');
          if (body) body.style.display = 'block';
          if (btn) btn.textContent = '▾';
          if (window.ensureFileBodyLoaded && body) {
            await window.ensureFileBodyLoaded(body);
          }
        }
        const expandBtn = skip.querySelector('.skip-all') || skip.querySelector('button');
        if (expandBtn && window.expandContextAll) {
          await window.expandContextAll(expandBtn);
        }
        if (!el) {
          if (jumpSide === 'L') {
            el = document.querySelector(`tr[data-line-l="${lineNum}"]`) ||
              document.getElementById(hash.slice(1)) ||
              document.querySelector(`tr[data-line-r="${lineNum}"]`);
          } else {
            el = document.getElementById(hash.slice(1)) ||
              document.querySelector(`tr[data-line-r="${lineNum}"]`) ||
              document.querySelector(`tr[data-line-l="${lineNum}"]`);
          }
        }
      }
    }

    if (el) {
      const container = (el.closest && (el.closest('.diff-container') || el.closest('.diff-file-section'))) || document;
      let effectiveSide = jumpSide || '';
      if (!effectiveSide) {
        const hasL = el.hasAttribute && el.hasAttribute('data-line-l') && parseInt(el.getAttribute('data-line-l') || '0', 10) === lineNum;
        const hasR = el.hasAttribute && el.hasAttribute('data-line-r') && parseInt(el.getAttribute('data-line-r') || '0', 10) === lineNum;
        if (hasL && !hasR) effectiveSide = 'L';
        else if (hasR && !hasL) effectiveSide = 'R';
      }

      const matchedRows = [];
      if (maxLine > lineNum) {
        const lineAttr = (jumpSide === 'L' || effectiveSide === 'L') ? 'data-line-l' :
          ((jumpSide === 'R' || effectiveSide === 'R') ? 'data-line-r' : null);
        const allDiffRows = container.querySelectorAll ? container.querySelectorAll('tr.diff-row') : [];
        for (let i = 0; i < allDiffRows.length; i++) {
          const r = allDiffRows[i];
          if (!r || !r.getAttribute) continue;
          let ln = 0;
          if (lineAttr) {
            ln = parseInt(r.getAttribute(lineAttr) || '0', 10);
          } else {
            const lr = parseInt(r.getAttribute('data-line-r') || '0', 10);
            const ll = parseInt(r.getAttribute('data-line-l') || '0', 10);
            ln = (lr >= lineNum && lr <= maxLine) ? lr : ll;
          }
          if (ln >= lineNum && ln <= maxLine) {
            matchedRows.push(r);
          }
        }
      }

      if (matchedRows.length === 0) {
        matchedRows.push(el);
      }

      if (el.scrollIntoView) {
        el.scrollIntoView({ block: 'center' });
      }
      flashDiffRows(matchedRows, effectiveSide);
    }
  }
  window.scrollToHashLine = scrollToHashLine;
  window.addEventListener('hashchange', scrollToHashLine);

  async function jumpToMovePeerTarget(target, e) {
    if (!target) return;
    const peerPath = (target.peerPath || '').replace(/^\/+/, '');
    const peerStart = parseInt(target.peerStart || '0', 10);
    const peerEnd = parseInt(target.peerEnd || '0', 10);
    const peerSide = target.peerSide || 'R';
    const repo = target.repo || '';
    const change = target.change || '';
    const sp = new URLSearchParams(window.location.search);
    sp.set('side', peerSide);
    if (peerEnd > peerStart) {
      sp.set('end', String(peerEnd));
    } else {
      sp.delete('end');
    }
    if (!sp.get('mode')) {
      const curMode = getCurrentPageDiffMode();
      if (curMode) sp.set('mode', curMode);
    }
    sp.delete('diffs');
    const cleanTarget = encodeURIComponent(peerPath).replace(/%2F/g, '/');
    const peerUrl = `/r/${encodeURIComponent(repo)}/c/${encodeURIComponent(change)}/f/${cleanTarget}?${sp.toString()}#L${peerStart}`;

    if (e) {
      if (e.button !== 0 || e.shiftKey || e.altKey) return;
      if (e.metaKey || e.ctrlKey) {
        e.preventDefault();
        e.stopPropagation();
        window.open(peerUrl, '_blank', 'noopener');
        return;
      }
      e.preventDefault();
      e.stopPropagation();
    }

    // Check if target file is in the current DOM (including lazy-unloaded sections in All Diffs)
    let targetContainer = null;
    const allSections = document.querySelectorAll('.diff-file-section');
    for (const sec of allSections) {
      const p = (sec.getAttribute('data-file-path') || '').replace(/^\/+/, '');
      if (p === peerPath) {
        const body = sec.querySelector('.diff-file-body');
        const chevron = sec.querySelector('.file-collapse-btn');
        if (body && (body.style.display === 'none' || sec.classList.contains('collapsed'))) {
          sec.classList.remove('collapsed');
          body.style.display = 'block';
          if (chevron) chevron.textContent = '▾';
          saveExpandedFiles();
          if (typeof window.updateToggleAllFilesBtn === 'function') {
            window.updateToggleAllFilesBtn();
          }
        }
        if (body && window.ensureFileBodyLoaded) {
          await window.ensureFileBodyLoaded(body);
        }
        targetContainer = sec.querySelector('.diff-container');
        break;
      }
    }
    if (!targetContainer) {
      const allContainers = document.querySelectorAll('.diff-container');
      for (const c of allContainers) {
        const p = (c.getAttribute('data-file-path') || '').replace(/^\/+/, '');
        if (p === peerPath) {
          targetContainer = c;
          break;
        }
      }
    }

    if (targetContainer) {
      const lineAttr = peerSide === 'L' ? 'data-line-l' : 'data-line-r';
      const rangeStart = peerStart;
      const rangeEnd = Math.max(peerStart, peerEnd || peerStart);

      // Expand any collapsed tr.row-skip overlapping [rangeStart, rangeEnd] on peerSide
      const skipRows = targetContainer.querySelectorAll('tr.row-skip');
      for (const skip of skipRows) {
        const lStart = parseInt(skip.getAttribute('data-left-start') || '0', 10);
        const lEnd = parseInt(skip.getAttribute('data-left-end') || '0', 10);
        const rStart = parseInt(skip.getAttribute('data-right-start') || '0', 10);
        const rEnd = parseInt(skip.getAttribute('data-right-end') || '0', 10);

        const skipStart = peerSide === 'L' ? lStart : rStart;
        const skipEnd = peerSide === 'L' ? lEnd : rEnd;
        if (skipStart > 0 && skipEnd > 0) {
          if (Math.max(rangeStart, skipStart) <= Math.min(rangeEnd, skipEnd)) {
            const expandBtn = skip.querySelector('.skip-all') || skip.querySelector('button');
            if (expandBtn && window.expandContextAll) {
              await window.expandContextAll(expandBtn);
            }
          }
        }
      }

      const matchedRows = [];
      let startRow = null;
      const allDiffRows = targetContainer.querySelectorAll ? targetContainer.querySelectorAll('tr.diff-row') : [];
      for (let i = 0; i < allDiffRows.length; i++) {
        const row = allDiffRows[i];
        if (!row || !row.getAttribute) continue;
        const ln = parseInt(row.getAttribute(lineAttr) || '0', 10);
        if (ln === rangeStart && !startRow) {
          startRow = row;
        }
        if (ln >= rangeStart && ln <= rangeEnd) {
          matchedRows.push(row);
        }
      }

      if (!startRow) {
        startRow = matchedRows[0] ||
          targetContainer.querySelector(`tr.diff-row[${lineAttr}="${rangeStart}"]`) ||
          targetContainer.querySelector(`tr[${lineAttr}="${rangeStart}"]`);
      }

      if (startRow && startRow.scrollIntoView) {
        startRow.scrollIntoView({ block: 'center' });
      }

      if (matchedRows.length > 0) {
        flashDiffRows(matchedRows, peerSide);
        return;
      } else if (startRow) {
        flashDiffRows(startRow, peerSide);
        return;
      }
    }

    // Target file is NOT in the current DOM (e.g. single-file view or not loaded), navigate:
    window.location.href = peerUrl;
  }
  window.jumpToMovePeerTarget = jumpToMovePeerTarget;

  async function handleJumpToMovePeer(e, badgeEl) {
    if (!badgeEl) return;

    const container = badgeEl.closest ? badgeEl.closest('.diff-container') : null;
    const allFilesEl = document.querySelector ? document.querySelector('.diff-all-files') : null;
    const currentPath = container ? (container.getAttribute('data-file-path') || '').replace(/^\/+/, '') : '';
    let peerPath = (badgeEl.getAttribute('data-peer-path') || '').replace(/^\/+/, '');
    if (!peerPath) {
      peerPath = currentPath;
    }
    const peerStart = parseInt(badgeEl.getAttribute('data-peer-start') || '0', 10);
    const peerEnd = parseInt(badgeEl.getAttribute('data-peer-end') || '0', 10);
    const peerSide = badgeEl.getAttribute('data-peer-side') || 'R';

    const repo = (container && container.getAttribute('data-repo')) || (allFilesEl && allFilesEl.getAttribute('data-repo')) || '';
    const change = (container && container.getAttribute('data-change')) || (allFilesEl && allFilesEl.getAttribute('data-change')) || '';

    await jumpToMovePeerTarget({
      repo,
      change,
      peerPath,
      peerStart,
      peerEnd,
      peerSide,
    }, e);
  }
  window.handleJumpToMovePeer = handleJumpToMovePeer;

  function initGlobalButtons() {
    updateThemeToggleBtn();
    updateSyntaxToggleBtn();
    restoreExpandedFiles();
    if (window.updateToggleAllFilesBtn) {
      window.updateToggleAllFilesBtn();
    }
    if (typeof enhanceChangeUnresolvedBadge === 'function') {
      enhanceChangeUnresolvedBadge();
    }
    if (typeof loadServerFormDrafts === 'function') {
      loadServerFormDrafts();
    }
    if (typeof restoreSavedDrafts === 'function') {
      restoreSavedDrafts();
    }
    scrollToHashLine();
    if (typeof observeAppHeaderResize === 'function') {
      observeAppHeaderResize();
    }
    if (typeof scheduleStickyScopeUpdate === 'function') {
      scheduleStickyScopeUpdate();
    }
  }

  // Update global buttons immediately and on DOMContentLoaded
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initGlobalButtons);
  } else {
    initGlobalButtons();
  }

  // SSE Live Reload
  let eventSource = null;
  let suppressSSEReloadUntil = 0;
  let staleBannerTimer = null;
  let pageStateSig = document.documentElement.getAttribute('data-state-sig') || '';

  function beginSelfReload() {
    suppressSSEReloadUntil = Date.now() + 5000;
    if (staleBannerTimer) {
      clearTimeout(staleBannerTimer);
      staleBannerTimer = null;
    }
    const banner = document.getElementById('updateBanner');
    if (banner) banner.classList.remove('show');
  }

  function cancelSelfReload() {
    suppressSSEReloadUntil = 0;
  }

  function closeSSE() {
    if (eventSource) {
      eventSource.close();
      eventSource = null;
    }
  }

  function initSSE() {
    if (!window.EventSource || document.hidden || eventSource) return;
    eventSource = new EventSource('/api/events');

    eventSource.addEventListener('reload', (e) => {
      if (Date.now() < suppressSSEReloadUntil) return;
      let evtSig = '';
      if (e && e.data) {
        try {
          const parsed = JSON.parse(e.data);
          if (parsed && parsed.sig) evtSig = parsed.sig;
        } catch (_) {}
      }

      const banner = document.getElementById('updateBanner');

      if (evtSig && pageStateSig && evtSig === pageStateSig) {
        if (staleBannerTimer) {
          clearTimeout(staleBannerTimer);
          staleBannerTimer = null;
        }
        if (banner) banner.classList.remove('show');
        return;
      }

      if (banner && banner.classList.contains('show')) {
        return;
      }

      if (!staleBannerTimer) {
        const delay = window.REVIEW_STALE_DELAY_MS ?? 300;
        staleBannerTimer = setTimeout(async () => {
          staleBannerTimer = null;
          if (Date.now() < suppressSSEReloadUntil) return;
          try {
            const res = await fetch('/api/state', { cache: 'no-store' });
            if (res.ok) {
              const data = await res.json();
              const sig = data && data.sig;
              if (sig && (!pageStateSig || sig !== pageStateSig)) {
                if (banner) banner.classList.add('show');
              } else {
                if (banner) banner.classList.remove('show');
              }
            }
          } catch (_) {}
        }, delay);
      }
    });

    eventSource.onerror = () => {
      closeSSE();
      if (!document.hidden) {
        setTimeout(initSSE, 5000);
      }
    };
  }

  let lastStateCheckAt = 0;
  async function checkRemoteState() {
    if (document.hidden) return;
    if (Date.now() < suppressSSEReloadUntil) return;
    if (Date.now() - lastStateCheckAt < 1500) return;
    lastStateCheckAt = Date.now();
    try {
      const m = window.location.pathname.match(/^\/r\/([^/]+)/);
      const stateUrl = m ? '/api/state?repo=' + encodeURIComponent(m[1]) : '/api/state';
      const res = await fetch(stateUrl, { cache: 'no-store' });
      if (res.ok) {
        const data = await res.json();
        const sig = data && data.sig;
        const banner = document.getElementById('updateBanner');
        if (sig && (!pageStateSig || sig !== pageStateSig)) {
          if (banner) banner.classList.add('show');
        } else {
          if (banner) banner.classList.remove('show');
        }
      }
    } catch (_) {}
  }

  document.addEventListener('visibilitychange', async () => {
    if (document.hidden) {
      closeSSE();
    } else {
      await checkRemoteState();
      initSSE();
    }
  });

  window.addEventListener('focus', checkRemoteState);

  window.addEventListener('pagehide', closeSSE);
  window.addEventListener('beforeunload', closeSSE);

  initSSE();

  // Auto-resize compact comment textareas as the user types
  function autoGrowTextarea(el) {
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.max(36, el.scrollHeight) + 'px';
  }

  // Diff selection tracking and strict side isolation (kept across mouseup so selection never bleeds)
  let selectedStart = 0;
  let selectedEnd = 0;
  let selectedStartCol = 0;
  let selectedEndCol = 0;
  let selectedSide = 'R';
  let selectedContainer = null;
  let activeRow = null;

  function getCharOffsetInElement(root, targetNode, targetOffset) {
    if (!root || !targetNode) return 0;
    try {
      const r = document.createRange();
      r.setStart(root, 0);
      r.setEnd(targetNode, targetOffset);
      return r.toString().length;
    } catch (e) {
      let charCount = 0;
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, null, false);
      let textNode;
      const elemNodeType = (typeof Node !== 'undefined' && Node.ELEMENT_NODE) ? Node.ELEMENT_NODE : 1;
      while ((textNode = walker.nextNode())) {
        if (textNode === targetNode || (targetNode.nodeType === elemNodeType && targetNode.contains && targetNode.contains(textNode))) {
          return charCount + targetOffset;
        }
        charCount += textNode.nodeValue.length;
      }
      return charCount;
    }
  }

  function highlightRangeInElement(td, startCol, endCol) {
    if (!td || startCol >= endCol) return;
    const walker = document.createTreeWalker(td, NodeFilter.SHOW_TEXT, null, false);
    const nodes = [];
    let n;
    while ((n = walker.nextNode())) {
      nodes.push(n);
    }
    let cur = 0;
    for (const node of nodes) {
      const len = node.nodeValue.length;
      const nodeStart = cur;
      const nodeEnd = cur + len;
      cur += len;

      if (nodeEnd <= startCol || nodeStart >= endCol) {
        continue;
      }
      const overlapStart = Math.max(0, startCol - nodeStart);
      const overlapEnd = Math.min(len, endCol - nodeStart);
      if (overlapStart >= overlapEnd) continue;

      let targetNode = node;
      if (overlapEnd < len) {
        targetNode.splitText(overlapEnd);
      }
      if (overlapStart > 0) {
        targetNode = targetNode.splitText(overlapStart);
      }
      const span = document.createElement('span');
      span.className = 'comment-range-hl';
      targetNode.parentNode.insertBefore(span, targetNode);
      span.appendChild(targetNode);
    }
  }

  function clearRangeHighlights() {
    document.querySelectorAll('td.comment-range-line').forEach(td => {
      td.classList.remove('comment-range-line');
    });
    const touchedParents = new Set();
    document.querySelectorAll('span.comment-range-hl').forEach(span => {
      const parent = span.parentNode;
      if (parent) {
        touchedParents.add(parent);
        while (span.firstChild) {
          parent.insertBefore(span.firstChild, span);
        }
        span.remove();
      }
    });
    touchedParents.forEach(p => p.normalize());
  }

  function highlightThreadRange(card) {
    if (!card) return;
    const hasRange = card.getAttribute('data-has-range') === 'true';
    if (!hasRange) return;

    const side = card.getAttribute('data-range-side') || 'R';
    const startLine = parseInt(card.getAttribute('data-start-line') || '0', 10);
    const endLine = parseInt(card.getAttribute('data-end-line') || '0', 10);
    const startCol = parseInt(card.getAttribute('data-start-col') || '0', 10);
    const endCol = parseInt(card.getAttribute('data-end-col') || '0', 10);

    if (startLine <= 0 || endLine <= 0) return;
    // Plain single whole-line comment: must NOT highlight on hover
    if (startLine === endLine && startCol === 0 && endCol === 0) return;

    let container = card.closest('.diff-container');
    if (!container) {
      const filePath = card.getAttribute('data-file-path');
      if (filePath) {
        container = document.querySelector(`.diff-container[data-file-path="${filePath}"]`) ||
                    document.querySelector(`.diff-container[data-file-path="/${filePath}"]`);
      }
    }
    if (!container) {
      container = document.querySelector('.diff-container') || document;
    }

    const getCodeCell = (line) => {
      const lineAttr = side === 'L' ? 'data-line-l' : 'data-line-r';
      const row = container.querySelector(`tr.diff-row[${lineAttr}="${line}"]`);
      if (!row) return null;
      return row.querySelector(`td[data-side="${side}"]`) || row.querySelector('td.line-code-unified') || row.querySelector('td.line-code');
    };

    if (startLine === endLine) {
      if (startCol > 0 || endCol > 0) {
        const td = getCodeCell(startLine);
        if (td) highlightRangeInElement(td, startCol, endCol);
      }
    } else if (startLine < endLine) {
      if (startCol === 0 && endCol === 0) {
        for (let l = startLine; l <= endLine; l++) {
          const td = getCodeCell(l);
          if (td) td.classList.add('comment-range-line');
        }
      } else {
        const startTd = getCodeCell(startLine);
        if (startTd) {
          const totalLen = (startTd.textContent || '').length;
          highlightRangeInElement(startTd, startCol, totalLen);
        }
        for (let l = startLine + 1; l < endLine; l++) {
          const midTd = getCodeCell(l);
          if (midTd) midTd.classList.add('comment-range-line');
        }
        const endTd = getCodeCell(endLine);
        if (endTd) {
          highlightRangeInElement(endTd, 0, endCol);
        }
      }
    }
  }

  let activeHighlightedCard = null;

  document.addEventListener('mouseover', (e) => {
    const card = e.target.closest('.thread-card[data-has-range="true"]');
    if (!card) return;
    if (activeHighlightedCard === card) return;
    clearRangeHighlights();
    activeHighlightedCard = card;
    highlightThreadRange(card);
  });

  document.addEventListener('mouseout', (e) => {
    if (!activeHighlightedCard) return;
    if (e.relatedTarget && activeHighlightedCard.contains(e.relatedTarget)) {
      return;
    }
    clearRangeHighlights();
    activeHighlightedCard = null;
  });

  window.addEventListener('pointerdown', (e) => {
    const inComment = e.target.closest('.thread-card, .comment-form');
    if (inComment) {
      const table = inComment.closest('.diff-table');
      if (table) table.classList.remove('selecting-L', 'selecting-R');
      return;
    }

    const codeTd = e.target.closest('td[data-side]');
    const gutterTd = e.target.closest('td[data-gutter-side]');
    const side = codeTd ? codeTd.getAttribute('data-side') : (gutterTd ? gutterTd.getAttribute('data-gutter-side') : null);
    if (!side) return;

    const table = (codeTd || gutterTd).closest('.diff-table');
    if (!table) return;

    const wantClass = side === 'L' ? 'selecting-L' : 'selecting-R';
    const otherClass = side === 'L' ? 'selecting-R' : 'selecting-L';
    if (table.classList.contains(otherClass) && !gutterTd) {
      const sel = window.getSelection();
      if (sel && !sel.isCollapsed) sel.removeAllRanges();
    }
    table.classList.remove(otherClass);
    table.classList.add(wantClass);
  }, true);

  // Shared Z-axis and popup state across comments, popups, and diff selections
  const openSymbolPopups = []; // strictly creation order: [oldest, ..., newest]
  window.openSymbolPopups = openSymbolPopups;
  let activePopupInst = null;
  let lastPointerX = -1;
  let lastPointerY = -1;
  window.setActivePopupInst = (inst) => { activePopupInst = inst; };

  function findTopmostPopupAtPoint(clientX, clientY) {
    if (typeof clientX !== 'number' || typeof clientY !== 'number') return null;
    let topInst = null;
    let topZ = -1;
    for (const inst of openSymbolPopups) {
      if (!inst || !inst.el || !inst.el.getBoundingClientRect) continue;
      const r = inst.el.getBoundingClientRect();
      if (clientX >= r.left && clientX <= r.right && clientY >= r.top && clientY <= r.bottom) {
        const z = inst._zOrder || parseInt(inst.el.style && inst.el.style.zIndex, 10) || 200;
        if (z >= topZ) {
          topZ = z;
          topInst = inst;
        }
      }
    }
    return topInst;
  }

  let nextPopupZIndex = 200;
  let selectedRangeZOrder = 199;
  let popupIdCounter = 0;

  function normalizeZIndices() {
    const items = [];
    for (const inst of openSymbolPopups) {
      if (inst && inst.el) {
        items.push({
          type: 'popup',
          inst: inst,
          z: inst._zOrder || parseInt(inst.el.style.zIndex, 10) || 200,
        });
      }
    }
    const elevatedCards = Array.from(document.querySelectorAll('.thread-card.comment-z-active'));
    for (const card of elevatedCards) {
      items.push({
        type: 'card',
        el: card,
        z: card._zOrder || parseInt(card.style.zIndex, 10) || 200,
      });
    }
    const drafts = [
      document.getElementById('inlineNewComment'),
      ...Array.from(document.querySelectorAll('.reply-form, .msg-edit-form')),
    ].filter(Boolean);
    for (const draft of drafts) {
      if (draft._zOrder) {
        items.push({
          type: 'draft',
          el: draft,
          z: draft._zOrder,
        });
      }
    }

    items.sort((a, b) => a.z - b.z);
    let baseZ = 200;
    for (const item of items) {
      const newZ = baseZ++;
      if (item.type === 'popup') {
        item.inst._zOrder = newZ;
        item.inst.el.style.zIndex = `${newZ}`;
      } else if (item.type === 'card') {
        item.el._zOrder = newZ;
        item.el.style.zIndex = `${newZ}`;
      } else if (item.type === 'draft') {
        item.el._zOrder = newZ;
      }
    }
    nextPopupZIndex = baseZ;
  }

  function bringPopupToFront(inst) {
    if (!inst || !inst.el) return;
    const currentZ = parseInt(inst.el.style.zIndex, 10);
    if (currentZ === nextPopupZIndex && nextPopupZIndex > 200 && inst._zOrder === nextPopupZIndex) {
      return;
    }
    nextPopupZIndex++;
    if (nextPopupZIndex >= 880) {
      normalizeZIndices();
    }
    inst._zOrder = nextPopupZIndex;
    inst.el.style.zIndex = `${nextPopupZIndex}`;
  }

  function bringCommentToFront(cardEl, draftEl) {
    if (!cardEl) return;
    nextPopupZIndex++;
    if (nextPopupZIndex >= 880) {
      normalizeZIndices();
    }
    const z = nextPopupZIndex;
    if (cardEl.classList && cardEl.classList.add) {
      cardEl.classList.add('comment-z-active');
    }
    if (cardEl.style) {
      cardEl.style.zIndex = `${z}`;
    }
    cardEl._zOrder = z;

    if (draftEl) {
      draftEl._zOrder = z;
    }
    const inlineEl = cardEl.closest ? cardEl.closest('#inlineNewComment') : null;
    if (inlineEl) {
      inlineEl._zOrder = z;
    }
    if (cardEl.querySelectorAll) {
      const openDrafts = cardEl.querySelectorAll('.reply-form, .msg-edit-form');
      openDrafts.forEach(d => {
        if (d.style && d.style.display !== 'none' && d.style.display !== '') {
          d._zOrder = z;
        }
      });
    }
  }

  function cleanupCommentZIndicesIfNeeded() {
    const hasInline = !!document.getElementById('inlineNewComment');
    const replyForms = Array.from(document.querySelectorAll('.reply-form'));
    const hasOpenReply = replyForms.some(rf => rf.style && rf.style.display !== 'none' && rf.style.display !== '');
    const editForms = Array.from(document.querySelectorAll('.msg-edit-form'));
    const hasOpenEdit = editForms.some(isEditFormVisible);
    const hasFlashing = document.querySelectorAll('.thread-card.jump-target-flash').length > 0;
    if (openSymbolPopups.length === 0 && !hasInline && !hasOpenReply && !hasOpenEdit && !hasFlashing) {
      document.querySelectorAll('.thread-card.comment-z-active').forEach(c => {
        if (c.classList && c.classList.remove) c.classList.remove('comment-z-active');
        if (c.style) c.style.zIndex = '';
        delete c._zOrder;
      });
      nextPopupZIndex = 200;
    }
  }

  function getPopupMinWidth(popup) {
    const MIN_WIDTH = 260;
    const LOC_MIN_WIDTH = 84;
    if (!popup) return MIN_WIDTH;
    const actionsEl = popup.querySelector ? popup.querySelector('.sym-popup-actions') : null;
    const backEl = popup.querySelector ? popup.querySelector('.sym-btn-back') : null;
    const actionsW = actionsEl ? (actionsEl.offsetWidth || 0) : 0;
    const backW = backEl ? (backEl.offsetWidth || 0) : 0;
    const gapsAndPad = 22 + (actionsW > 0 ? 8 : 0) + (backW > 0 ? 8 : 0);
    const clientW = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1000;
    return Math.min(Math.max(180, clientW - 24), Math.max(MIN_WIDTH, LOC_MIN_WIDTH + actionsW + backW + gapsAndPad));
  }
  window.getPopupMinWidth = getPopupMinWidth;

  function ensurePopupMinWidth(popupOrInst) {
    const inst = popupOrInst && popupOrInst.el ? popupOrInst : null;
    if (inst && inst.resizedW && inst.el) {
      const minW = getPopupMinWidth(inst.el);
      const curW = parseFloat(inst.el.style.width) || inst.el.offsetWidth || 0;
      if (curW > 0 && curW < minW) {
        inst.el.style.width = `${minW}px`;
      }
    }
  }

  function blurFocusedPopupIfAny() {
    const ae = document.activeElement;
    if (ae && ae.closest && ae.closest('.symbol-popup') && ae.blur) {
      try { ae.blur(); } catch (_) {}
    }
  }

  function getFocusedSymbolPopup(activeEl) {
    if (activePopupInst && openSymbolPopups.includes(activePopupInst)) {
      return activePopupInst;
    }
    if (lastPointerX < 0 && activeEl && activeEl.closest) {
      const popupEl = activeEl.closest('.symbol-popup');
      if (popupEl) {
        return openSymbolPopups.find(p => p && p.el === popupEl) || null;
      }
    }
    return null;
  }

  function closeAllSymbolPopups() {
    let closedAny = false;
    const popupsCopy = openSymbolPopups.slice();
    for (const inst of popupsCopy) {
      if (closeSpecificSymbolPopup(inst)) {
        closedAny = true;
      }
    }
    if (document.querySelectorAll) {
      const strayPopups = Array.from(document.querySelectorAll('.symbol-popup, #symbolPopup'));
      for (const p of strayPopups) {
        if (p && p.remove) {
          p.remove();
          closedAny = true;
        }
      }
    }
    activePopupInst = null;
    return closedAny;
  }
  window.closeAllSymbolPopups = closeAllSymbolPopups;

  function isPopupPersistent(inst) {
    return !!(inst && (inst.moved || inst.resizedW || inst.resizedH || inst.snapped || inst.shaded || inst.usedShade));
  }
  window.isPopupPersistent = isPopupPersistent;

  function dismissUnpinnedPopupsExcept(keepInst) {
    let closedAny = false;
    const popupsCopy = openSymbolPopups.slice();
    for (const inst of popupsCopy) {
      if (!inst || inst === keepInst || isPopupPersistent(inst)) {
        continue;
      }
      if (closeSpecificSymbolPopup(inst)) {
        closedAny = true;
      }
    }
    if (!keepInst && document.querySelectorAll) {
      const strayPopups = Array.from(document.querySelectorAll('.symbol-popup, #symbolPopup'));
      for (const p of strayPopups) {
        if (p && !openSymbolPopups.some(inst => inst && inst.el === p) && p.remove) {
          p.remove();
          closedAny = true;
        }
      }
    }
    return closedAny;
  }

  function triggerEscapeAction(activeEl, opts) {
    opts = opts || {};
    if (opts.shiftKey) {
      const focusedInst = getFocusedSymbolPopup(activeEl);
      if (focusedInst) {
        closeSpecificSymbolPopup(focusedInst);
        return true;
      }
      return closeAllSymbolPopups();
    }

    const unresMenu = document.querySelector('.unresolved-threads-menu');
    if (unresMenu && unresMenu.style.display !== 'none') {
      if (typeof closeUnresolvedMenu === 'function') {
        closeUnresolvedMenu();
      } else {
        unresMenu.style.display = 'none';
      }
      return true;
    }
    const modal = document.getElementById('shortcutsModal');
    if (modal && (modal.style.display === 'flex' || modal.style.display === 'block')) {
      modal.style.display = 'none';
      return true;
    }

    if (activeEl) {
      const tag = activeEl.tagName;
      const isInput = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || (activeEl.isContentEditable === true);
      if (isInput) {
        if (activeEl.blur) activeEl.blur();
        return true;
      }
    }

    const focusedInst = opts.forceClosePopups ? null : getFocusedSymbolPopup(activeEl);

    if (focusedInst) {
      bringPopupToFront(focusedInst);
      if (stepPopupBackOrCancel(focusedInst)) {
        return true;
      }
      closeSpecificSymbolPopup(focusedInst);
      return true;
    }

    const sel = window.getSelection ? window.getSelection() : null;
    const inlineEl = document.getElementById('inlineNewComment');
    if (sel && !sel.isCollapsed) {
      if (!inlineEl && selectedStart === 0) {
        if (sel.removeAllRanges) sel.removeAllRanges();
        return true;
      }
    }

    const closables = [];

    for (const inst of openSymbolPopups) {
      if (inst && inst.el && (inst.el.isConnected !== false && inst.el.parentElement)) {
        const z = inst._zOrder || parseInt(inst.el.style.zIndex, 10) || 200;
        closables.push({
          z,
          close: () => closeSpecificSymbolPopup(inst),
        });
      }
    }
    const domPopups = Array.from(document.querySelectorAll('.symbol-popup'));
    for (const p of domPopups) {
      if (!openSymbolPopups.some(inst => inst && inst.el === p)) {
        const z = p._zOrder || parseInt(p.style && p.style.zIndex, 10) || 200;
        closables.push({
          z,
          close: () => {
            if (p.remove) p.remove();
          },
        });
      }
    }

    if (inlineEl) {
      if (!inlineEl._zOrder) {
        bringCommentToFront(inlineEl.querySelector ? (inlineEl.querySelector('.thread-card') || inlineEl) : inlineEl, inlineEl);
      }
      const z = inlineEl._zOrder || 200;
      closables.push({
        z,
        close: () => closeInlineComment(false),
      });
    }

    const replyForms = Array.from(document.querySelectorAll('.reply-form'));
    for (const rf of replyForms) {
      if (rf.style && rf.style.display !== 'none' && rf.style.display !== '') {
        if (!rf._zOrder) {
          bringCommentToFront(rf.closest ? (rf.closest('.thread-card') || rf) : rf, rf);
        }
        const z = rf._zOrder || 200;
        const threadID = rf.id ? rf.id.replace('reply-form-', '') : '';
        closables.push({
          z,
          close: () => {
            const ta = rf.querySelector('textarea');
            if (ta) ta.value = '';
            if (threadID && window.toggleReplyForm) {
              window.toggleReplyForm(threadID, false);
            } else {
              rf.style.display = 'none';
              const rfCard = rf.closest ? rf.closest('.thread-card') : null;
              if (rfCard && rfCard.classList) rfCard.classList.remove('reply-open');
              restoreThreadCardResolvedState(rfCard);
              cleanupCommentZIndicesIfNeeded();
              if (typeof handleNaturalPageHeightChange === 'function') handleNaturalPageHeightChange();
            }
          },
        });
      }
    }

    const msgEditForms = Array.from(document.querySelectorAll('.msg-edit-form'));
    for (const ef of msgEditForms) {
      if (isEditFormVisible(ef)) {
        if (!ef._zOrder) {
          bringCommentToFront(ef.closest ? (ef.closest('.thread-card') || ef) : ef, ef);
        }
        const z = ef._zOrder || 200;
        const msgID = ef.id ? ef.id.replace('msg-edit-', '') : '';
        closables.push({
          z,
          close: () => {
            if (msgID && window.closeEditMessageForm) {
              window.closeEditMessageForm(msgID, false);
            } else {
              ef.style.display = 'none';
              const mb = (ef.closest && ef.closest('.message-body')) || (msgID ? document.getElementById('msg-' + msgID) : null);
              if (mb && mb.classList) mb.classList.remove('editing-msg');
              restoreThreadCardResolvedState(ef.closest ? ef.closest('.thread-card') : null);
              cleanupCommentZIndicesIfNeeded();
              if (typeof handleNaturalPageHeightChange === 'function') handleNaturalPageHeightChange();
            }
          },
        });
      }
    }

    if (!inlineEl && (selectedStart > 0 || (document.querySelector && document.querySelector('.comment-range-line, .comment-range-hl')))) {
      const z = selectedRangeZOrder || 199;
      closables.push({
        z,
        close: () => {
          if (sel && sel.removeAllRanges) sel.removeAllRanges();
          closeInlineComment();
        },
      });
    }

    if (closables.length > 0) {
      let winner = closables[0];
      for (let i = 1; i < closables.length; i++) {
        if (closables[i].z >= winner.z) {
          winner = closables[i];
        }
      }
      winner.close();
      return true;
    }

    return false;
  }
  window.triggerEscapeAction = triggerEscapeAction;

  document.addEventListener('pointerdown', (e) => {
    if (typeof e.clientX === 'number') {
      lastPointerX = e.clientX;
      lastPointerY = e.clientY;
    }
    const targetEl = e.target && e.target.nodeType === 3 ? e.target.parentElement : e.target;
    const popupEl = targetEl && targetEl.closest ? (targetEl.closest('.symbol-popup') || targetEl.closest('#symbolPopup')) : null;
    const isPrimary = (e.button === undefined || e.button === 0);
    if (popupEl) {
      const inst = openSymbolPopups.find(p => p && p.el === popupEl);
      if (inst) {
        if (isPrimary) {
          dismissUnpinnedPopupsExcept(inst);
        }
        activePopupInst = inst;
        if (inst.el && inst.el.focus) inst.el.focus({ preventScroll: true });
        return;
      }
    }
    if (isPrimary) {
      dismissUnpinnedPopupsExcept(null);
    }
    activePopupInst = null;
    blurFocusedPopupIfAny();
  }, true);

  document.addEventListener('pointermove', (e) => {
    if (e.clientX === lastPointerX && e.clientY === lastPointerY) return;
    lastPointerX = e.clientX;
    lastPointerY = e.clientY;
    const topInst = findTopmostPopupAtPoint(e.clientX, e.clientY);
    activePopupInst = topInst || null;
    if (!topInst) blurFocusedPopupIfAny();
  }, { passive: true, capture: true });

  const chunkNavState = { el: null, scrollY: -1 };
  const threadNavState = { el: null, scrollY: -1 };

  document.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    const card = e.target.closest ? e.target.closest('.thread-card') : null;
    if (card) {
      bringCommentToFront(card);
      if (card.classList && !card.classList.contains('new-comment-card')) {
        threadNavState.el = card;
        threadNavState.scrollY = window.scrollY || 0;
      }
    }
  });

  document.addEventListener('focusin', (e) => {
    const card = e.target.closest ? e.target.closest('.thread-card') : null;
    if (card) {
      bringCommentToFront(card);
      if (card.classList && !card.classList.contains('new-comment-card')) {
        threadNavState.el = card;
        threadNavState.scrollY = window.scrollY || 0;
      }
    }
  });

  function captureDiffSelection() {
    const sel = window.getSelection();
    if (!sel || sel.rangeCount === 0 || sel.isCollapsed) {
      return null;
    }
    const range = sel.getRangeAt(0);

    const findDiffRow = (node) => {
      let cur = node && node.nodeType === Node.ELEMENT_NODE ? node : node?.parentElement;
      while (cur && cur !== document.body) {
        if (cur.tagName === 'TR' && (cur.hasAttribute('data-line-r') || cur.hasAttribute('data-line-l'))) {
          return cur;
        }
        cur = cur.parentElement;
      }
      return null;
    };

    let startTr = findDiffRow(range.startContainer);
    let endTr = findDiffRow(range.endContainer);
    if (!startTr || !endTr) return null;

    const table = startTr.closest('.diff-table');
    let side = 'R';
    if (table && table.classList.contains('selecting-L')) {
      side = 'L';
    } else if (table && table.classList.contains('selecting-R')) {
      side = 'R';
    } else {
      const startTd = (range.startContainer.nodeType === Node.ELEMENT_NODE ? range.startContainer : range.startContainer.parentElement)?.closest('td[data-side]');
      side = startTd ? startTd.getAttribute('data-side') : (startTr.getAttribute('data-line-r') ? 'R' : 'L');
    }

    const lineAttr = side === 'L' ? 'data-line-l' : 'data-line-r';
    let startLine = parseInt(startTr.getAttribute(lineAttr) || '0', 10);
    let endLine = parseInt(endTr.getAttribute(lineAttr) || '0', 10);

    if (startLine <= 0 && endLine <= 0) return null;
    if (startLine <= 0 && endLine > 0) {
      startTr = endTr;
      startLine = endLine;
    } else if (endLine <= 0 && startLine > 0) {
      endTr = startTr;
      endLine = startLine;
    }

    let startCodeTd = startTr.querySelector(`td[data-side="${side}"]`) || startTr.querySelector('td.line-code-unified') || startTr.querySelector('td.line-code');
    let endCodeTd = endTr.querySelector(`td[data-side="${side}"]`) || endTr.querySelector('td.line-code-unified') || endTr.querySelector('td.line-code');

    if (!startCodeTd || !endCodeTd) return null;

    let startCol = getCharOffsetInElement(startCodeTd, range.startContainer, range.startOffset);
    let endCol = getCharOffsetInElement(endCodeTd, range.endContainer, range.endOffset);

    // Line boundary check: if user double-clicked or dragged to end of line,
    // browser may place endContainer at 0 offset of next line.
    if (endTr !== startTr && endCol === 0) {
      let prevTr = endTr.previousElementSibling;
      while (prevTr && (!prevTr.classList.contains('diff-row') || !prevTr.getAttribute(lineAttr))) {
        prevTr = prevTr.previousElementSibling;
      }
      if (prevTr) {
        endTr = prevTr;
        endLine = parseInt(endTr.getAttribute(lineAttr) || '0', 10);
        endCodeTd = endTr.querySelector(`td[data-side="${side}"]`) || endTr.querySelector('td.line-code-unified') || endTr.querySelector('td.line-code');
        endCol = endCodeTd ? (endCodeTd.textContent || '').length : 0;
      }
    }

    if (startLine > endLine) {
      const tmpLine = startLine; startLine = endLine; endLine = tmpLine;
      const tmpCol = startCol; startCol = endCol; endCol = tmpCol;
      const tmpTr = startTr; startTr = endTr; endTr = tmpTr;
    } else if (startLine === endLine && startCol > endCol) {
      const tmpCol = startCol; startCol = endCol; endCol = tmpCol;
    }

    const container = startTr.closest('.diff-container');
    selectedStart = startLine;
    selectedEnd = endLine;
    selectedStartCol = startCol;
    selectedEndCol = endCol;
    selectedSide = side;
    selectedContainer = container;
    activeRow = endTr;
    if (selectedStart > 0) {
      selectedRangeZOrder = ++nextPopupZIndex;
    }

    return {
      side,
      startLine,
      endLine,
      startCol,
      endCol,
      container,
      anchorRow: endTr
    };
  }

  document.addEventListener('mouseup', (e) => {
    if (e.button !== 0) return;
    const sel = window.getSelection();
    if (!sel || sel.isCollapsed) {
      if (!e.target.closest('td.line-num:not(.skip-gutter)')) {
        selectedStart = 0;
        selectedEnd = 0;
        selectedStartCol = 0;
        selectedEndCol = 0;
        selectedContainer = null;
      }
      return;
    }
    captureDiffSelection();
  });

  function getCurrentRepoAndChange(contextEl) {
    let repo = '';
    let change = '';
    if (contextEl) {
      const container = contextEl.closest ? contextEl.closest('.diff-container') : null;
      if (container && container.getAttribute) {
        repo = container.getAttribute('data-repo') || '';
        change = container.getAttribute('data-change') || '';
      }
    }
    if (!repo || !change) {
      const countsEl = document.getElementById('changeCommentCounts');
      if (countsEl && countsEl.getAttribute) {
        repo = repo || countsEl.getAttribute('data-repo') || '';
        change = change || countsEl.getAttribute('data-change') || '';
      }
    }
    if (!repo || !change) {
      const allFilesEl = document.querySelector ? document.querySelector('.diff-all-files') : null;
      if (allFilesEl && allFilesEl.getAttribute) {
        repo = repo || allFilesEl.getAttribute('data-repo') || '';
        change = change || allFilesEl.getAttribute('data-change') || '';
      }
    }
    if (!repo || !change) {
      const anyCont = document.querySelector ? document.querySelector('.diff-container') : null;
      if (anyCont && anyCont.getAttribute) {
        repo = repo || anyCont.getAttribute('data-repo') || '';
        change = change || anyCont.getAttribute('data-change') || '';
      }
    }
    if (!repo || !change) {
      const overviewEl = document.querySelector ? document.querySelector('.change-overview') : null;
      if (overviewEl && overviewEl.getAttribute) {
        repo = repo || overviewEl.getAttribute('data-repo') || '';
        change = change || overviewEl.getAttribute('data-change') || '';
      }
    }
    if ((!repo || !change) && window.location) {
      const m = (window.location.pathname || '').match(/\/r\/([^/]+)\/c\/([^/?#]+)/);
      if (m) {
        repo = repo || decodeURIComponent(m[1]);
        change = change || decodeURIComponent(m[2]);
      }
    }
    return { repo, change };
  }

  function findFormDraft(key) {
    if (!key) return null;
    if (formDraftsMap.has(key)) return formDraftsMap.get(key);
    for (const [k, v] of formDraftsMap.entries()) {
      if (k === key) return v;
      if (v && v.draft_key === key) return v;
      if ((k.startsWith('edit:') || k.startsWith('reply:')) && key.endsWith(':' + k)) return v;
      if ((key.startsWith('edit:') || key.startsWith('reply:')) && k.endsWith(':' + key)) return v;
    }
    return null;
  }

  function saveFormDraftServer(draftKey, payload) {
    if (draftSaveTimers.has(draftKey)) {
      clearTimeout(draftSaveTimers.get(draftKey));
      draftSaveTimers.delete(draftKey);
    }
    formDraftsMap.set(draftKey, payload);
    const timer = setTimeout(() => {
      draftSaveTimers.delete(draftKey);
      const info = getCurrentRepoAndChange();
      if (!info.repo || !info.change) return;
      fetch(`/api/form-drafts?repo=${encodeURIComponent(info.repo)}&change=${encodeURIComponent(info.change)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        keepalive: true,
        body: JSON.stringify({
          repo: info.repo,
          change: info.change,
          action: 'save',
          draft_key: draftKey,
          target_path: payload.target_path || '',
          side: payload.side || '',
          line: payload.line || 0,
          end_line: payload.end_line || payload.endLine || 0,
          start_col: payload.start_col || payload.startCol || 0,
          end_col: payload.end_col || payload.endCol || 0,
          unresolved: payload.unresolved !== undefined ? !!payload.unresolved : true,
          thread_id: (payload.thread_id != null && payload.thread_id !== '') ? String(payload.thread_id) : '',
          message_id: (payload.message_id != null && payload.message_id !== '') ? String(payload.message_id) : '',
          content: payload.content || '',
        })
      }).catch(() => {});
    }, 150);
    draftSaveTimers.set(draftKey, timer);
  }

  function deleteFormDraftServer(draftKey, meta) {
    if (draftSaveTimers.has(draftKey)) {
      clearTimeout(draftSaveTimers.get(draftKey));
      draftSaveTimers.delete(draftKey);
    }
    formDraftsMap.delete(draftKey);
    const info = getCurrentRepoAndChange();
    if (!info.repo || !info.change) return;
    fetch(`/api/form-drafts?repo=${encodeURIComponent(info.repo)}&change=${encodeURIComponent(info.change)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      keepalive: true,
      body: JSON.stringify({
        repo: info.repo,
        change: info.change,
        action: 'delete',
        draft_key: draftKey,
        thread_id: (meta && meta.thread_id != null && meta.thread_id !== '') ? String(meta.thread_id) : '',
        message_id: (meta && meta.message_id != null && meta.message_id !== '') ? String(meta.message_id) : '',
      })
    }).catch(() => {});
  }

  async function loadServerFormDrafts() {
    const info = getCurrentRepoAndChange();
    if (!info.repo || !info.change) return;
    try {
      const res = await fetch(`/api/form-drafts?repo=${encodeURIComponent(info.repo)}&change=${encodeURIComponent(info.change)}`);
      if (!res.ok) return;
      const data = await res.json();
      if (data && Array.isArray(data.drafts)) {
        for (const d of data.drafts) {
          if (d && d.draft_key) {
            if (draftSaveTimers.has(d.draft_key)) {
              continue;
            }
            formDraftsMap.set(d.draft_key, {
              draft_key: d.draft_key,
              target_path: d.target_path || '',
              side: d.side || '',
              line: d.line || 0,
              end_line: d.end_line || 0,
              start_col: d.start_col || 0,
              end_col: d.end_col || 0,
              unresolved: d.unresolved !== undefined ? !!d.unresolved : true,
              thread_id: d.thread_id || '',
              message_id: d.message_id || '',
              content: d.content || '',
              body: d.content || '',
            });
          }
        }
        if (typeof restoreSavedDrafts === 'function') {
          restoreSavedDrafts();
        }
      }
    } catch (_) {}
  }

  function flushPendingDrafts() {
    if (draftSaveTimers.size === 0) return;
    const info = getCurrentRepoAndChange();
    if (!info.repo || !info.change) return;
    for (const [draftKey, timer] of draftSaveTimers.entries()) {
      clearTimeout(timer);
      const payload = formDraftsMap.get(draftKey);
      if (!payload || !payload.content) continue;
      const bodyStr = JSON.stringify({
        repo: info.repo,
        change: info.change,
        action: 'save',
        draft_key: draftKey,
        target_path: payload.target_path || '',
        side: payload.side || '',
        line: payload.line || 0,
        end_line: payload.end_line || payload.endLine || 0,
        start_col: payload.start_col || payload.startCol || 0,
        end_col: payload.end_col || payload.endCol || 0,
        unresolved: payload.unresolved !== undefined ? !!payload.unresolved : true,
        thread_id: (payload.thread_id != null && payload.thread_id !== '') ? String(payload.thread_id) : '',
        message_id: (payload.message_id != null && payload.message_id !== '') ? String(payload.message_id) : '',
        content: payload.content || '',
      });
      const url = `/api/form-drafts?repo=${encodeURIComponent(info.repo)}&change=${encodeURIComponent(info.change)}`;
      let sent = false;
      if (typeof navigator !== 'undefined' && navigator.sendBeacon) {
        try {
          const blob = new Blob([bodyStr], { type: 'application/json' });
          sent = navigator.sendBeacon(url, blob);
        } catch (_) {}
      }
      if (!sent && typeof fetch !== 'undefined') {
        try {
          fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: bodyStr,
            keepalive: true
          }).catch(() => {});
        } catch (_) {}
      }
    }
    draftSaveTimers.clear();
  }

  window.addEventListener('beforeunload', flushPendingDrafts);
  window.addEventListener('pagehide', flushPendingDrafts);

  function pruneOldDrafts() {
    try {
      if (typeof localStorage === 'undefined') return;
      const toRemove = [];
      for (let i = 0; i < localStorage.length; i++) {
        const k = localStorage.key(i);
        if (k && k.startsWith('review_draft:')) {
          toRemove.push(k);
        }
      }
      for (const k of toRemove) {
        localStorage.removeItem(k);
      }
    } catch (_) {}
  }

  function getReplyDraftKey(threadID, formEl) {
    const info = getCurrentRepoAndChange(formEl);
    return `review_draft:reply:${info.repo}:${info.change}:${threadID}`;
  }

  function getEditDraftKey(msgID, formEl) {
    const info = getCurrentRepoAndChange(formEl);
    return `review_draft:edit:${info.repo}:${info.change}:${msgID}`;
  }

  window.handleInlineCommentInput = function(ta) {
    autoGrowTextarea(ta);
    const formTr = ta.closest('#inlineNewComment');
    const key = formTr ? formTr.getAttribute('data-draft-key') : null;
    if (!key) return;
    const card = formTr.querySelector('.thread-card');
    const container = formTr.closest('.diff-container');
    const filePath = (container && container.getAttribute('data-file-path')) || '';
    const cleanPath = filePath.replace(/^\/+/, '') || filePath;
    const side = (card && card.getAttribute('data-range-side')) || selectedSide || 'R';
    const startLine = card ? parseInt(card.getAttribute('data-start-line') || '0', 10) : 0;
    const endLine = card ? parseInt(card.getAttribute('data-end-line') || '0', 10) : startLine;
    const startCol = card ? parseInt(card.getAttribute('data-start-col') || '0', 10) : 0;
    const endCol = card ? parseInt(card.getAttribute('data-end-col') || '0', 10) : 0;
    const resCb = formTr.querySelector('input[name="resolved"]');
    const unresolved = resCb ? !resCb.checked : true;

    if (!ta.value.trim()) {
      deleteFormDraftServer(key, { target_path: cleanPath, side: side, line: startLine });
      return;
    }
    saveFormDraftServer(key, {
      draft_key: key,
      target_path: cleanPath,
      side: side,
      line: startLine,
      content: ta.value,
      end_line: endLine,
      start_col: startCol,
      end_col: endCol,
      unresolved: unresolved,
    });
  };

  window.handleInlineCommentResolvedChange = function(cb) {
    syncCommentBoxResolved(cb);
    const formTr = cb.closest('#inlineNewComment');
    const key = formTr ? formTr.getAttribute('data-draft-key') : null;
    if (!key) return;
    const form = cb.closest('form');
    const ta = form ? form.querySelector('textarea') : null;
    if (!ta || !ta.value.trim()) return;
    const card = form ? form.closest('.thread-card') : null;
    const container = formTr.closest('.diff-container');
    const filePath = (container && container.getAttribute('data-file-path')) || '';
    const cleanPath = filePath.replace(/^\/+/, '') || filePath;
    const side = (card && card.getAttribute('data-range-side')) || selectedSide || 'R';
    const startLine = card ? parseInt(card.getAttribute('data-start-line') || '0', 10) : 0;
    const endLine = card ? parseInt(card.getAttribute('data-end-line') || '0', 10) : startLine;
    const startCol = card ? parseInt(card.getAttribute('data-start-col') || '0', 10) : 0;
    const endCol = card ? parseInt(card.getAttribute('data-end-col') || '0', 10) : 0;
    saveFormDraftServer(key, {
      draft_key: key,
      target_path: cleanPath,
      side: side,
      line: startLine,
      content: ta.value,
      end_line: endLine,
      start_col: startCol,
      end_col: endCol,
      unresolved: !cb.checked,
    });
  };

  function isElementVisible(el) {
    if (!el || !el.style) return false;
    return el.style.display !== 'none' && el.style.display !== '';
  }

  function isEditFormVisible(ef) {
    if (!isElementVisible(ef)) return false;
    if (ef.closest && ef.closest('.message-body.msg-collapsed')) return false;
    const msgBody = ef.closest ? ef.closest('.message-body') : null;
    if (msgBody && !(msgBody.classList && msgBody.classList.contains('message-reply'))) {
      const card = msgBody.closest ? msgBody.closest('.thread-card') : null;
      if (card && card.classList && card.classList.contains('first-msg-collapsed')) {
        return false;
      }
    }
    return true;
  }

  function getCardOrigUnresolved(card) {
    if (!card) return null;
    if (card.hasAttribute && card.hasAttribute('data-resolved')) {
      return card.getAttribute('data-resolved') !== 'true';
    }
    if (card.classList) {
      return card.classList.contains('thread-unresolved');
    }
    return null;
  }

  function syncCommentBoxResolved(cb) {
    if (!cb) return;
    const card = cb.closest ? (cb.closest('.new-comment-card') || cb.closest('.thread-card')) : null;
    if (!card) return;
    const resolved = !!cb.checked;
    if (card.classList) {
      if (!resolved) {
        card.classList.add('thread-unresolved');
      } else {
        card.classList.remove('thread-unresolved');
      }
    }
    const isNewCard = card.classList && card.classList.contains('new-comment-card');
    if (!isNewCard && card.querySelectorAll) {
      card.querySelectorAll('input[name="resolved"]').forEach(sib => {
        if (sib !== cb) sib.checked = resolved;
      });
    }
  }
  window.syncCommentBoxResolved = syncCommentBoxResolved;

  function restoreThreadCardResolvedState(card) {
    if (!card) return;
    if (card.querySelectorAll) {
      const openForms = Array.from(card.querySelectorAll('.reply-form, .msg-edit-form')).filter(isElementVisible);
      for (let i = openForms.length - 1; i >= 0; i--) {
        const openCb = openForms[i].querySelector ? openForms[i].querySelector('input[name="resolved"]') : null;
        if (openCb) {
          window.syncCommentBoxResolved(openCb);
          return;
        }
      }
    }
    if (card.hasAttribute && card.hasAttribute('data-resolved')) {
      const origResolved = card.getAttribute('data-resolved') === 'true';
      if (card.querySelectorAll) {
        card.querySelectorAll('input[name="resolved"]').forEach(cb => {
          cb.checked = origResolved;
        });
      }
      if (card.classList) {
        if (!origResolved) {
          card.classList.add('thread-unresolved');
        } else {
          card.classList.remove('thread-unresolved');
        }
      }
    }
  }

  function refreshLayoutAfterCollapseChange() {
    if (typeof handleNaturalPageHeightChange === 'function') handleNaturalPageHeightChange();
    if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
    if (typeof scheduleStickyScopeUpdate === 'function') scheduleStickyScopeUpdate();
  }

  function setCommentCollapsed(msgIDOrEl, collapsed, opts) {
    if (msgIDOrEl === null || msgIDOrEl === undefined || msgIDOrEl === '') return false;
    const msgEl = typeof msgIDOrEl === 'object'
      ? msgIDOrEl
      : document.getElementById('msg-' + msgIDOrEl);
    if (!msgEl) return false;
    const nextCollapsed = !!collapsed;
    if (opts && opts.userToggled && msgEl.setAttribute) {
      msgEl.setAttribute('data-user-toggled', 'true');
    }
    const card = msgEl.closest
      ? msgEl.closest('.thread-card')
      : (msgEl.parentElement && msgEl.parentElement.classList && msgEl.parentElement.classList.contains('thread-card') ? msgEl.parentElement : null);
    const isFirst = !(msgEl.classList && msgEl.classList.contains('message-reply'));
    const wasCollapsed = !!(msgEl.classList && msgEl.classList.contains('msg-collapsed'));
    const wasFirstCollapsed = (isFirst && card && card.classList)
      ? card.classList.contains('first-msg-collapsed')
      : nextCollapsed;
    if (wasCollapsed === nextCollapsed && wasFirstCollapsed === nextCollapsed) {
      return false;
    }
    if (!(opts && opts.userToggled) && msgEl.removeAttribute) {
      msgEl.removeAttribute('data-user-toggled');
    }

    if (nextCollapsed && typeof document !== 'undefined' && document.activeElement) {
      const ae = document.activeElement;
      if (msgEl.contains && msgEl.contains(ae)) {
        let focusHidden = isFirst;
        if (!focusHidden && ae.closest) {
          const hiddenPart = ae.closest('.msg-content, .msg-edit-form');
          if (hiddenPart && msgEl.contains(hiddenPart)) {
            focusHidden = true;
          }
        }
        if (focusHidden && ae.blur) {
          try { ae.blur(); } catch (_) {}
        }
      }
    }

    if (msgEl.classList) {
      if (nextCollapsed) msgEl.classList.add('msg-collapsed');
      else msgEl.classList.remove('msg-collapsed');
    }
    if (isFirst && card && card.classList) {
      if (nextCollapsed) card.classList.add('first-msg-collapsed');
      else card.classList.remove('first-msg-collapsed');
    }
    const header = isFirst
      ? (card && card.querySelector ? card.querySelector('.thread-header') : null)
      : (msgEl.querySelector ? msgEl.querySelector('.reply-header') : null);
    if (header) {
      if (header.setAttribute) {
        header.setAttribute('data-collapsed', nextCollapsed ? 'true' : 'false');
      }
      const btn = header.querySelector ? header.querySelector('.comment-collapse-btn') : null;
      if (btn && btn.setAttribute) {
        const label = nextCollapsed ? 'Expand comment' : 'Collapse comment';
        btn.setAttribute('aria-expanded', nextCollapsed ? 'false' : 'true');
        btn.setAttribute('aria-label', label);
        btn.setAttribute('title', label);
      }
    }
    if (msgEl.classList && msgEl.classList.contains('editing-msg')) {
      if (nextCollapsed) {
        if (typeof cleanupCommentZIndicesIfNeeded === 'function') {
          cleanupCommentZIndicesIfNeeded();
        }
      } else {
        const openEf = msgEl.querySelector ? msgEl.querySelector('.msg-edit-form') : null;
        if (openEf && isEditFormVisible(openEf) && typeof bringCommentToFront === 'function') {
          bringCommentToFront(card, openEf);
        }
      }
    }
    if (!(opts && opts.batch)) {
      refreshLayoutAfterCollapseChange();
    }
    return true;
  }
  window.setCommentCollapsed = setCommentCollapsed;

  function toggleCommentCollapsed(msgID) {
    const msgEl = document.getElementById('msg-' + msgID);
    if (!msgEl) return false;
    const isCollapsed = !!(msgEl.classList && msgEl.classList.contains('msg-collapsed'));
    return setCommentCollapsed(msgEl, !isCollapsed, { userToggled: true });
  }
  window.toggleCommentCollapsed = toggleCommentCollapsed;

  function handleCommentHeaderClick(event, msgID) {
    if (event && event.button !== undefined && event.button !== 0) {
      return;
    }
    const rawTarget = event && event.target ? event.target : null;
    const targetEl = rawTarget && rawTarget.nodeType === 3 ? rawTarget.parentElement : rawTarget;
    if (targetEl && typeof targetEl.closest === 'function') {
      if (targetEl.closest('.thread-actions, button:not(.comment-collapse-btn), a, input, label, textarea, select')) {
        return;
      }
    }
    const clickedToggleBtn = !!(targetEl && typeof targetEl.closest === 'function' && targetEl.closest('.comment-collapse-btn'));
    if (!clickedToggleBtn && typeof window.getSelection === 'function') {
      const sel = window.getSelection();
      if (sel && !sel.isCollapsed && String(sel || '').length > 0) {
        return;
      }
    }
    toggleCommentCollapsed(msgID);
  }
  window.handleCommentHeaderClick = handleCommentHeaderClick;

  function setThreadCommentsCollapsed(card, collapsed, opts) {
    if (!card || !card.querySelectorAll) return false;
    let anyChanged = false;
    const childOpts = { batch: true, userToggled: !!(opts && opts.userToggled) };
    const bodies = card.querySelectorAll('.message-body');
    for (const mb of bodies) {
      if (setCommentCollapsed(mb, collapsed, childOpts)) {
        anyChanged = true;
      }
    }
    if (anyChanged && !(opts && opts.batch)) {
      refreshLayoutAfterCollapseChange();
    }
    return anyChanged;
  }
  window.setThreadCommentsCollapsed = setThreadCommentsCollapsed;

  function setAllCommentsCollapsed(collapsed) {
    if (!document.querySelectorAll) return false;
    let anyChanged = false;
    const cards = document.querySelectorAll('.thread-card');
    for (const card of cards) {
      if (card.classList && card.classList.contains('new-comment-card')) continue;
      if (setThreadCommentsCollapsed(card, collapsed, { batch: true, userToggled: true })) {
        anyChanged = true;
      }
    }
    if (anyChanged) {
      refreshLayoutAfterCollapseChange();
    }
    return anyChanged;
  }
  window.setAllCommentsCollapsed = setAllCommentsCollapsed;

  function revealThreadForJump(card, messageId, opts) {
    if (!card) return false;
    if (messageId) {
      const candidate = document.getElementById('msg-' + messageId);
      if (candidate && (!card.contains || card.contains(candidate))) {
        return setCommentCollapsed(candidate, false, opts);
      }
    }
    if (card.querySelectorAll) {
      const bodies = card.querySelectorAll('.message-body');
      for (const mb of bodies) {
        if (!mb.classList || !mb.classList.contains('msg-collapsed')) {
          return false;
        }
      }
    }
    return setThreadCommentsCollapsed(card, false, opts);
  }
  window.revealThreadForJump = revealThreadForJump;

  function replaceThreadCardHTML(card, html, opts) {
    if (!card || !html) return null;
    let collapsedByMsgID = null;
    if (opts && (opts.preserveCollapsed || opts.preservePublishedCollapsed) && card.querySelectorAll) {
      const skipDrafts = !!(opts.preservePublishedCollapsed && !opts.preserveCollapsed);
      collapsedByMsgID = new Map();
      card.querySelectorAll('.message-body').forEach(mb => {
        if (!mb || !mb.id || !mb.getAttribute || mb.getAttribute('data-user-toggled') !== 'true') return;
        if (skipDrafts && mb.querySelector && mb.querySelector('.msg-edit-form')) return;
        collapsedByMsgID.set(mb.id, !!(mb.classList && mb.classList.contains('msg-collapsed')));
      });
    }
    const parent = card.parentElement || card.parentNode;
    const cardID = card.id;
    let newCard = null;
    if (card.replaceWith && typeof document.createElement === 'function') {
      const temp = document.createElement('div');
      temp.innerHTML = String(html).trim();
      newCard = temp.firstElementChild || (temp.children && temp.children[0]) || null;
      if (newCard) {
        card.replaceWith(newCard);
      }
    }
    if (!newCard) {
      card.outerHTML = html;
      newCard = cardID ? document.getElementById(cardID) : null;
    }
    const restoreRoot = newCard || parent;
    if (collapsedByMsgID && collapsedByMsgID.size > 0 && restoreRoot && restoreRoot.querySelectorAll) {
      restoreRoot.querySelectorAll('.message-body').forEach(newMb => {
        if (newMb && newMb.id && collapsedByMsgID.has(newMb.id)) {
          setCommentCollapsed(newMb, collapsedByMsgID.get(newMb.id), { batch: true, userToggled: true });
        }
      });
    }
    if (restoreRoot && typeof restoreSavedDrafts === 'function') {
      restoreSavedDrafts(restoreRoot);
    }
    if (!(opts && opts.batch)) {
      refreshLayoutAfterCollapseChange();
    }
    return newCard;
  }

  function syncThreadDraftsUnresolved(threadID, nextUnresolved, card, threadData) {
    if (threadID == null || threadID === '') return;
    const threadMsgIDs = new Set();
    const origBodyByMsgID = new Map();
    if (card && card.querySelectorAll) {
      card.querySelectorAll('.msg-edit-form').forEach(ef => {
        if (ef && ef.id && ef.id.startsWith('msg-edit-')) {
          const mid = ef.id.slice('msg-edit-'.length);
          if (mid) {
            threadMsgIDs.add(mid);
            const orig = ef.getAttribute ? ef.getAttribute('data-orig-body') : null;
            if (orig !== null) origBodyByMsgID.set(mid, orig);
          }
        }
      });
      card.querySelectorAll('.message-body').forEach(mb => {
        if (mb && mb.id && mb.id.startsWith('msg-')) {
          const mid = mb.id.slice('msg-'.length);
          if (mid) threadMsgIDs.add(mid);
        }
      });
    }
    if (threadData && Array.isArray(threadData.messages)) {
      for (const m of threadData.messages) {
        if (m && m.id != null) {
          const mid = String(m.id);
          threadMsgIDs.add(mid);
          if (!origBodyByMsgID.has(mid) && m.body != null) {
            origBodyByMsgID.set(mid, String(m.body));
          }
        }
      }
    }

    const tidStr = String(threadID);
    for (const [k, d] of Array.from(formDraftsMap.entries())) {
      if (!d) continue;
      const dTid = (d.thread_id != null && d.thread_id !== '') ? String(d.thread_id) : '';
      const dMid = (d.message_id != null && d.message_id !== '') ? String(d.message_id) : '';
      const isThreadReply = dTid === tidStr ||
        (!dMid && (k === `reply:${tidStr}` || (k.startsWith('review_draft:reply:') && k.endsWith(`:${tidStr}`))));
      let mid = dMid;
      if (!mid && !dTid && (k.startsWith('review_draft:edit:') || k.startsWith('edit:'))) {
        mid = k.slice(k.lastIndexOf(':') + 1);
      }
      const isThreadEdit = !isThreadReply && mid !== '' && threadMsgIDs.has(mid);
      if (!isThreadReply && !isThreadEdit) continue;

      const prevUnresolved = d.unresolved;
      if (d.unresolved !== undefined) {
        d.unresolved = nextUnresolved;
      }
      if (isThreadEdit) {
        const orig = origBodyByMsgID.has(mid) ? origBodyByMsgID.get(mid) : null;
        const curBody = d.content !== undefined ? d.content : d.body;
        if (orig !== null && curBody === orig && (d.unresolved === undefined || d.unresolved === nextUnresolved)) {
          deleteFormDraftServer(k, { message_id: mid });
          continue;
        }
      }
      if (d.unresolved !== undefined && prevUnresolved !== nextUnresolved) {
        saveFormDraftServer(k, d);
      }
    }
  }

  // Open compact inline comment form strictly inside the active side's code column
  function openCommentForm(tr, line, side, fromShortcut, opts) {
    if (!tr || !line || line <= 0) return;
    if (tr.closest('.diff-container')?.getAttribute('data-readonly') === 'true') {
      return;
    }
    const container = tr.closest('.diff-container') || document;
    const existing = container.querySelector('#inlineNewComment') || document.getElementById('inlineNewComment');
    if (existing) existing.remove();

    const targetSide = side || selectedSide || 'R';
    const useRange = selectedStart > 0 && selectedSide === targetSide &&
                     selectedContainer === tr.closest('.diff-container') &&
                     (fromShortcut || (line >= selectedStart && line <= selectedEnd));
    const start = useRange ? selectedStart : line;
    const end = useRange ? selectedEnd : line;
    const startCol = useRange ? selectedStartCol : 0;
    const endCol = useRange ? selectedEndCol : 0;
    const anchorRow = (useRange && activeRow && activeRow.closest('.diff-container') === tr.closest('.diff-container')) ? activeRow : tr;

    const isSplit = anchorRow.closest('.diff-table-split') !== null;
    const formTr = document.createElement('tr');
    formTr.id = 'inlineNewComment';
    formTr.className = 'thread-row';

    const filePath = (container.getAttribute && container.getAttribute('data-file-path')) || '';
    const cleanPath = filePath.replace(/^\/+/, '') || filePath;
    const info = getCurrentRepoAndChange(container);
    const draftKey = `review_draft:new:${info.repo}:${info.change}:${cleanPath}:${targetSide}:${start}`;
    formTr.setAttribute('data-draft-key', draftKey);

    const hasRange = (start < end || startCol > 0 || endCol > 0);
    const rangeAttrs = (hasRange ? 'data-has-range="true" ' : '') +
      `data-range-side="${targetSide}" data-start-line="${start}" data-end-line="${end}" data-start-col="${startCol}" data-end-col="${endCol}"`;

    let locLabel = `L${start}`;
    if (start !== end) {
      locLabel = `L${start}–${end}`;
    }
    const formContent = `<div class="thread-card comment-form new-comment-card thread-unresolved" ${rangeAttrs}>` +
      `<form onsubmit="submitInlineComment(event, ${start}, ${end}, '${targetSide}', ${startCol}, ${endCol})">` +
        `<textarea id="inlineCommentText" rows="2" oninput="handleInlineCommentInput(this)" placeholder="Add a comment... (Markdown supported)" required></textarea>` +
        `<div class="comment-footer">` +
          `<div class="comment-footer-left">` +
            `<label class="resolved-toggle"><input type="checkbox" name="resolved" onchange="handleInlineCommentResolvedChange(this)"> Resolved</label>` +
            `<span class="comment-hint">${locLabel} · Cmd/Ctrl+Enter</span>` +
          `</div>` +
          `<div class="comment-actions">` +
            `<button type="button" class="thread-btn" onclick="closeInlineComment(true)">Cancel</button>` +
            `<button type="submit" class="thread-btn thread-btn-primary">Save</button>` +
          `</div>` +
        `</div>` +
      `</form>` +
    `</div>`;

    if (isSplit) {
      const lEmpty = !(anchorRow.getAttribute && anchorRow.getAttribute('data-line-l'));
      const rEmpty = !(anchorRow.getAttribute && anchorRow.getAttribute('data-line-r'));
      if (targetSide === 'L') {
        formTr.innerHTML = `<td class="line-num thread-gutter${lEmpty ? ' cell-empty' : ''}"></td><td class="thread-cell">${formContent}</td><td class="line-num thread-gutter${rEmpty ? ' cell-empty' : ''}"></td><td class="line-code${rEmpty ? ' cell-empty' : ''}"></td>`;
      } else {
        formTr.innerHTML = `<td class="line-num thread-gutter${lEmpty ? ' cell-empty' : ''}"></td><td class="line-code${lEmpty ? ' cell-empty' : ''}"></td><td class="line-num thread-gutter${rEmpty ? ' cell-empty' : ''}"></td><td class="thread-cell">${formContent}</td>`;
      }
    } else {
      const isSingle = anchorRow.closest('.diff-table-single') !== null;
      formTr.innerHTML = isSingle
        ? `<td class="line-num thread-gutter"></td><td class="thread-cell">${formContent}</td>`
        : `<td class="line-num thread-gutter"></td><td class="line-num thread-gutter"></td><td class="thread-cell">${formContent}</td>`;
    }

    anchorRow.parentNode.insertBefore(formTr, anchorRow.nextSibling);
    const cardEl = formTr.querySelector('.thread-card');
    bringCommentToFront(cardEl, formTr);
    const ta = formTr.querySelector('#inlineCommentText');
    const resCb = formTr.querySelector('input[name="resolved"]');
    const dVal = findFormDraft(draftKey);
    if (dVal && (dVal.content || dVal.body) && ta) {
      ta.value = dVal.content || dVal.body;
      autoGrowTextarea(ta);
      if (dVal.unresolved !== undefined && resCb) {
        resCb.checked = !dVal.unresolved;
        syncCommentBoxResolved(resCb);
      }
    }

    if (!opts || !opts.noFocus) {
      if (ta) ta.focus();
    }
    if (typeof handleNaturalPageHeightChange === 'function') {
      handleNaturalPageHeightChange();
    }
  }
  window.openCommentForm = openCommentForm;

  function closeInlineComment(explicitCancel) {
    clearRangeHighlights();
    const existing = document.getElementById('inlineNewComment');
    if (existing) {
      if (explicitCancel) {
        const key = existing.getAttribute('data-draft-key');
        if (key) {
          deleteFormDraftServer(key);
        }
      }
      existing.remove();
    }
    selectedStart = 0;
    selectedEnd = 0;
    selectedStartCol = 0;
    selectedEndCol = 0;
    selectedContainer = null;
    cleanupCommentZIndicesIfNeeded();
    if (typeof handleNaturalPageHeightChange === 'function') {
      handleNaturalPageHeightChange();
    }
  }
  window.closeInlineComment = closeInlineComment;

  window.submitInlineComment = function(e, start, end, side, startCol, endCol) {
    if (e && e.preventDefault) e.preventDefault();
    const form = (e && e.target && e.target.tagName === 'FORM' && e.target.querySelector)
      ? e.target
      : (document.querySelector('#inlineNewComment form') || (e && e.target && e.target.querySelector ? e.target : null));
    if (!form) return;
    const ta = form.querySelector('textarea');
    if (!ta || !ta.value.trim()) return;
    if (form._submitting) return;
    form._submitting = true;

    const resCb = form.querySelector('input[name="resolved"]');
    const resolved = resCb ? !!resCb.checked : false;

    const container = form.closest('.diff-container');
    const path = (container && container.getAttribute('data-file-path')) || '';
    const changeKey = (container && container.getAttribute('data-change')) || '';
    const repoSlug = (container && container.getAttribute('data-repo')) || '';
    const rev = side === 'L' ? (container ? (container.getAttribute('data-left-rev') || '') : '') : (container ? (container.getAttribute('data-right-rev') || '') : '');
    const leftRev = container ? (container.getAttribute('data-left-rev') || '') : '';
    const rightRev = container ? (container.getAttribute('data-right-rev') || '') : '';

    const inlineRow = (form.closest ? form.closest('#inlineNewComment') : null) || document.getElementById('inlineNewComment');
    const inlineKey = inlineRow ? inlineRow.getAttribute('data-draft-key') : null;
    const cleanP = (path || '').replace(/^\/+/, '') || path;
    const fbKey = `review_draft:new:${repoSlug}:${changeKey}:${cleanP}:${side}:${start}`;
    for (const k of [inlineKey, fbKey]) {
      if (k && draftSaveTimers.has(k)) {
        clearTimeout(draftSaveTimers.get(k));
        draftSaveTimers.delete(k);
      }
    }

    beginSelfReload();
    fetch('/api/threads/add', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        repo: repoSlug,
        change: changeKey,
        rev: rev,
        left_rev: leftRev,
        right_rev: rightRev,
        path: path,
        side: side,
        start_line: start,
        end_line: end,
        start_col: startCol || 0,
        end_col: endCol || 0,
        body: ta.value.trim(),
        resolved: resolved
      })
    }).then(res => {
      if (res.ok) return res.json();
      cancelSelfReload();
      throw new Error('failed');
    }).then(data => {
      if (!data) return;
      if (data.sig) pageStateSig = data.sig;
      suppressSSEReloadUntil = Date.now() + 300;
      clearRangeHighlights();
      const inlineRow = document.getElementById('inlineNewComment');
      if (inlineRow) {
        const key = inlineRow.getAttribute('data-draft-key');
        if (key) {
          deleteFormDraftServer(key);
        }
      }
      const cleanP = (path || '').replace(/^\/+/, '') || path;
      const fbKey = `review_draft:new:${repoSlug}:${changeKey}:${cleanP}:${side}:${start}`;
      deleteFormDraftServer(fbKey);
      if (inlineRow && data.html) {
        inlineRow.removeAttribute('id');
        const cell = inlineRow.querySelector('.thread-cell');
        if (cell) cell.innerHTML = data.html;
        refreshLayoutAfterCollapseChange();
      }
      selectedStart = 0;
      selectedEnd = 0;
      selectedStartCol = 0;
      selectedEndCol = 0;
      cleanupCommentZIndicesIfNeeded();
      updateCommentCountBadges(data.file_path, data.file_counts, data.change_counts);
    }).catch(() => {
      cancelSelfReload();
    }).finally(() => {
      form._submitting = false;
    });
  };

  window.openEditMessageForm = function(msgID, opts) {
    const content = document.getElementById('msg-content-' + msgID);
    const form = document.getElementById('msg-edit-' + msgID);
    let wasExpanded = false;
    if (content) content.style.display = 'none';
    if (form) {
      const msgBody = (form.closest && form.closest('.message-body')) || document.getElementById('msg-' + msgID);
      if (setCommentCollapsed(msgBody || msgID, false, { batch: true })) {
        wasExpanded = true;
      }
      if (msgBody && msgBody.classList) {
        msgBody.classList.add('editing-msg');
      }
      const draftKey = getEditDraftKey(msgID, form);
      form.setAttribute('data-draft-key', draftKey);
      const ta = form.querySelector ? form.querySelector('textarea') : null;
      const resCb = form.querySelector ? form.querySelector('input[name="resolved"]') : null;
      const dVal = findFormDraft(draftKey) || findFormDraft(`edit:${msgID}`);
      if (dVal && (dVal.content || dVal.body) && ta) {
        ta.value = dVal.content || dVal.body;
      }
      if (dVal && dVal.unresolved !== undefined && resCb) {
        resCb.checked = !dVal.unresolved;
        syncCommentBoxResolved(resCb);
      }
      form.style.display = 'block';
      if (ta) autoGrowTextarea(ta);
      const card = form.closest ? form.closest('.thread-card') : null;
      bringCommentToFront(card, form);
      if (!opts || !opts.noFocus) {
        if (ta && ta.focus) ta.focus();
      }
    }
    if (wasExpanded) {
      refreshLayoutAfterCollapseChange();
    } else if (typeof handleNaturalPageHeightChange === 'function') {
      handleNaturalPageHeightChange();
    }
  };

  window.closeEditMessageForm = function(msgID, explicitCancel) {
    const content = document.getElementById('msg-content-' + msgID);
    const form = document.getElementById('msg-edit-' + msgID);
    if (content) content.style.display = 'block';
    if (form) {
      if (explicitCancel) {
        const draftKey = form.getAttribute('data-draft-key') || getEditDraftKey(msgID, form);
        deleteFormDraftServer(draftKey, { message_id: msgID });
        if (draftSaveTimers.has(`edit:${msgID}`)) {
          clearTimeout(draftSaveTimers.get(`edit:${msgID}`));
          draftSaveTimers.delete(`edit:${msgID}`);
        }
        formDraftsMap.delete(`edit:${msgID}`);
      }
      const orig = form.getAttribute('data-orig-body');
      const ta = form.querySelector ? form.querySelector('textarea') : null;
      if (ta && orig !== null) ta.value = orig;
      form.style.display = 'none';
      const msgBody = (form.closest && form.closest('.message-body')) || document.getElementById('msg-' + msgID);
      if (msgBody && msgBody.classList) {
        msgBody.classList.remove('editing-msg');
      }
      restoreThreadCardResolvedState(form.closest ? form.closest('.thread-card') : null);
    }
    cleanupCommentZIndicesIfNeeded();
    if (typeof handleNaturalPageHeightChange === 'function') {
      handleNaturalPageHeightChange();
    }
  };

  function saveOrClearEditDraft(form, msgID) {
    if (!form) return;
    const ta = form.querySelector ? form.querySelector('textarea') : null;
    if (!ta) return;
    const draftKey = form.getAttribute('data-draft-key') || getEditDraftKey(msgID, form);
    const orig = form.getAttribute('data-orig-body');
    const card = form.closest ? form.closest('.thread-card') : null;
    const origUnresolved = getCardOrigUnresolved(card);
    const resCb = form.querySelector ? form.querySelector('input[name="resolved"]') : null;
    const unresolved = resCb ? !resCb.checked : origUnresolved;
    const bodyUnchanged = (orig !== null && ta.value === orig);
    const resUnchanged = (origUnresolved === null || unresolved === origUnresolved);
    if (!ta.value.trim() || (bodyUnchanged && resUnchanged)) {
      deleteFormDraftServer(draftKey, { message_id: msgID });
      return;
    }
    const draft = {
      draft_key: draftKey,
      message_id: String(msgID),
      content: ta.value,
    };
    if (resCb) {
      draft.unresolved = !resCb.checked;
    }
    saveFormDraftServer(draftKey, draft);
  }

  window.handleEditMsgInput = function(ta, msgID) {
    autoGrowTextarea(ta);
    const form = (ta && ta.closest ? ta.closest('.msg-edit-form') : null) || document.getElementById('msg-edit-' + msgID);
    saveOrClearEditDraft(form, msgID);
  };

  window.handleEditResolvedChange = function(cb, msgID) {
    syncCommentBoxResolved(cb);
    const form = (cb && cb.closest ? cb.closest('.msg-edit-form') : null) || document.getElementById('msg-edit-' + msgID);
    saveOrClearEditDraft(form, msgID);
  };

  window.submitEditMessage = function(eOrMsgID, maybeMsgID) {
    let msgID = eOrMsgID;
    if (eOrMsgID && typeof eOrMsgID === 'object') {
      if (eOrMsgID.preventDefault) eOrMsgID.preventDefault();
      msgID = maybeMsgID;
    } else if (maybeMsgID !== undefined) {
      msgID = maybeMsgID;
    }
    const form = document.getElementById('msg-edit-' + msgID);
    if (!form) return;
    const ta = form.querySelector ? form.querySelector('textarea') : null;
    if (!ta || !ta.value.trim()) return;
    if (form._submitting) return;
    form._submitting = true;

    const resCb = form.querySelector ? form.querySelector('input[name="resolved"]') : null;
    const container = form.closest ? form.closest('.diff-container') : null;
    const leftRev = container ? (container.getAttribute('data-left-rev') || '') : '';
    const rightRev = container ? (container.getAttribute('data-right-rev') || '') : '';

    const draftKey = form.getAttribute('data-draft-key') || getEditDraftKey(msgID, form);
    const editFbKey = `edit:${msgID}`;
    for (const k of [draftKey, editFbKey]) {
      if (k && draftSaveTimers.has(k)) {
        clearTimeout(draftSaveTimers.get(k));
        draftSaveTimers.delete(k);
      }
    }

    const payload = {
      message_id: Number(msgID) || msgID,
      body: ta.value.trim(),
      left_rev: leftRev,
      right_rev: rightRev,
    };
    if (resCb) {
      payload.resolved = !!resCb.checked;
    }

    beginSelfReload();
    fetch('/api/threads/edit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(res => {
      if (res.ok) return res.json();
      cancelSelfReload();
      throw new Error('failed');
    }).then(data => {
      if (!data) return;
      if (data.sig) pageStateSig = data.sig;
      suppressSSEReloadUntil = Date.now() + 300;
      clearRangeHighlights();
      const draftKey = form.getAttribute('data-draft-key') || getEditDraftKey(msgID, form);
      deleteFormDraftServer(draftKey, { message_id: msgID });
      formDraftsMap.delete(editFbKey);
      const threadID = data.thread ? data.thread.id : (data.message ? data.message.thread_id : null);
      const card = threadID ? document.getElementById('thread-' + threadID) : null;
      if (threadID != null && ((data.thread && typeof data.thread.resolved === 'boolean') || payload.resolved !== undefined)) {
        const nextUnresolved = (data.thread && typeof data.thread.resolved === 'boolean') ? !data.thread.resolved : !payload.resolved;
        syncThreadDraftsUnresolved(threadID, nextUnresolved, card, data.thread);
      }
      if (card && data.html) {
        replaceThreadCardHTML(card, data.html, { preserveCollapsed: true });
      }
      cleanupCommentZIndicesIfNeeded();
      updateCommentCountBadges(data.file_path, data.file_counts, data.change_counts);
    }).catch(() => {
      cancelSelfReload();
    }).finally(() => {
      form._submitting = false;
    });
  };

  // Reply Form Toggle
  window.toggleReplyForm = function(threadID, explicitCancel, opts) {
    const form = document.getElementById('reply-form-' + threadID);
    if (!form) return;
    const card = form.closest ? form.closest('.thread-card') : null;
    if (explicitCancel) {
      const draftKey = form.getAttribute('data-draft-key') || getReplyDraftKey(threadID, form);
      deleteFormDraftServer(draftKey, { thread_id: threadID });
      if (draftSaveTimers.has(`reply:${threadID}`)) {
        clearTimeout(draftSaveTimers.get(`reply:${threadID}`));
        draftSaveTimers.delete(`reply:${threadID}`);
      }
      formDraftsMap.delete(`reply:${threadID}`);
      const ta = form.querySelector ? form.querySelector('textarea') : null;
      if (ta) ta.value = '';
      form.style.display = 'none';
      if (card && card.classList) card.classList.remove('reply-open');
      restoreThreadCardResolvedState(card);
      cleanupCommentZIndicesIfNeeded();
      if (typeof handleNaturalPageHeightChange === 'function') {
        handleNaturalPageHeightChange();
      }
      return;
    }
    const isClosing = (form.style.display !== 'none' && form.style.display !== '');
    if (isClosing) {
      const ta = form.querySelector ? form.querySelector('textarea') : null;
      if (ta) ta.value = '';
      form.style.display = 'none';
      if (card && card.classList) card.classList.remove('reply-open');
      restoreThreadCardResolvedState(card);
      cleanupCommentZIndicesIfNeeded();
    } else {
      const draftKey = getReplyDraftKey(threadID, form);
      form.setAttribute('data-draft-key', draftKey);
      const ta = form.querySelector ? form.querySelector('textarea') : null;
      const resCb = form.querySelector ? form.querySelector('input[name="resolved"]') : null;
      const dVal = findFormDraft(draftKey) || findFormDraft(`reply:${threadID}`);
      if (dVal && (dVal.content || dVal.body) && ta) {
        ta.value = dVal.content || dVal.body;
      }
      if (dVal && dVal.unresolved !== undefined && resCb) {
        resCb.checked = !dVal.unresolved;
        syncCommentBoxResolved(resCb);
      }
      form.style.display = 'block';
      if (card && card.classList) card.classList.add('reply-open');
      if (ta) autoGrowTextarea(ta);
      bringCommentToFront(card, form);
      if (!opts || !opts.noFocus) {
        if (ta && ta.focus) ta.focus();
      }
    }
    if (typeof handleNaturalPageHeightChange === 'function') {
      handleNaturalPageHeightChange();
    }
  };

  window.handleReplyInput = function(ta, threadID) {
    autoGrowTextarea(ta);
    const form = ta.closest('.reply-form');
    const draftKey = (form && form.getAttribute('data-draft-key')) || getReplyDraftKey(threadID, form);
    if (!ta.value.trim()) {
      deleteFormDraftServer(draftKey, { thread_id: threadID });
      return;
    }
    const resCb = form ? form.querySelector('input[name="resolved"]') : null;
    const unresolved = resCb ? !resCb.checked : true;
    saveFormDraftServer(draftKey, {
      draft_key: draftKey,
      thread_id: String(threadID),
      content: ta.value,
      unresolved: unresolved,
    });
  };

  window.handleReplyResolvedChange = function(cb, threadID) {
    syncCommentBoxResolved(cb);
    const form = cb.closest('.reply-form');
    const ta = form ? form.querySelector('textarea') : null;
    if (!ta || !ta.value.trim()) return;
    const draftKey = (form && form.getAttribute('data-draft-key')) || getReplyDraftKey(threadID, form);
    saveFormDraftServer(draftKey, {
      draft_key: draftKey,
      thread_id: String(threadID),
      content: ta.value,
      unresolved: !cb.checked,
    });
  };

  // Enclosing declaration context in sticky file and app headers
  function getContainerScopes(container) {
    if (!container) return null;
    if (container._parsedScopes !== undefined) return container._parsedScopes;
    const raw = container.getAttribute ? container.getAttribute('data-scopes') : null;
    if (!raw) {
      container._parsedScopes = null;
      return null;
    }
    try {
      container._parsedScopes = JSON.parse(raw);
    } catch (e) {
      container._parsedScopes = null;
    }
    return container._parsedScopes;
  }

  function isSplitScopeContainer(container, table) {
    if (!container) return false;
    const mode = (container.getAttribute && container.getAttribute('data-diff-mode')) || '';
    if (mode !== 'split') return false;
    if (container.hasAttribute && container.hasAttribute('data-ext-pkg')) return false;
    if (container.getAttribute && container.getAttribute('data-ext-pkg')) return false;
    if (table && table.classList && table.classList.contains && table.classList.contains('diff-table-single')) return false;
    return true;
  }

  function getContainerScopesForSide(container, side) {
    if (!container) return null;
    if (!container._parsedScopesBySide) {
      container._parsedScopesBySide = {};
    }
    if (container._parsedScopesBySide[side] !== undefined) {
      return container._parsedScopesBySide[side];
    }
    let res = null;
    if (side === 'R') {
      const rawR = container.getAttribute ? container.getAttribute('data-scopes-r') : null;
      if (rawR !== null && rawR !== undefined && rawR !== '') {
        try {
          res = JSON.parse(rawR);
        } catch (e) {
          res = null;
        }
      } else {
        const rawL = container.getAttribute ? container.getAttribute('data-scopes-l') : null;
        const defSide = (container.getAttribute ? container.getAttribute('data-scopes-side') : null) || 'R';
        if (!rawL && defSide === 'R') {
          res = getContainerScopes(container);
        } else {
          res = null;
        }
      }
    } else if (side === 'L') {
      const rawL = container.getAttribute ? container.getAttribute('data-scopes-l') : null;
      if (rawL !== null && rawL !== undefined && rawL !== '') {
        try {
          res = JSON.parse(rawL);
        } catch (e) {
          res = null;
        }
      } else {
        const rawR = container.getAttribute ? container.getAttribute('data-scopes-r') : null;
        const defSide = container.getAttribute ? container.getAttribute('data-scopes-side') : null;
        if (!rawR && defSide === 'L') {
          res = getContainerScopes(container);
        } else {
          res = null;
        }
      }
    }
    container._parsedScopesBySide[side] = res;
    return res;
  }

  function getGlobalScopeSide() {
    return (document.documentElement && document.documentElement.getAttribute('data-scope-side') === 'L') ? 'L' : 'R';
  }

  function setGlobalScopeSide(nextSide) {
    const side = nextSide === 'L' ? 'L' : 'R';
    if (document.documentElement) {
      if (side === 'L') {
        document.documentElement.setAttribute('data-scope-side', 'L');
      } else {
        document.documentElement.removeAttribute('data-scope-side');
      }
    }
    try {
      localStorage.setItem('review_scope_side', side);
    } catch (e) {}
    if (typeof fetch === 'function') {
      fetch('/api/prefs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key: 'scope_side', val: side }),
      }).catch(() => {});
    }
    if (typeof updateStickyScopes === 'function') {
      updateStickyScopes();
    }
  }
  window.setGlobalScopeSide = setGlobalScopeSide;

  function getActiveContainerScopeSide(container, table) {
    if (isSplitScopeContainer(container, table)) {
      return getGlobalScopeSide();
    }
    return ((container && container.getAttribute ? container.getAttribute('data-scopes-side') : null) === 'L') ? 'L' : 'R';
  }

  function findScopeAtLine(scopes, line) {
    if (!scopes || !scopes.length || line <= 0) return null;
    let best = null;
    for (const s of scopes) {
      if (s.s < line && line <= s.e) {
        if (!best || s.s > best.s || (s.s === best.s && s.e < best.e)) {
          best = s;
        }
      }
    }
    return best;
  }

  var stickyScopeAnimFrame = null;
  function scheduleStickyScopeUpdate() {
    if (stickyScopeAnimFrame) return;
    const raf = typeof requestAnimationFrame === 'function' ? requestAnimationFrame : (cb) => setTimeout(cb, 16);
    stickyScopeAnimFrame = raf(() => {
      stickyScopeAnimFrame = null;
      updateStickyScopes();
    });
  }

  function resolveSideAnchorLine(rows, startIdx, attr) {
    if (!rows || rows.length === 0 || startIdx < 0 || startIdx >= rows.length) return 0;
    const curRow = rows[startIdx];
    const v = parseInt((curRow && curRow.getAttribute ? curRow.getAttribute(attr) : '0') || '0', 10);
    if (v > 0) return v;

    if (curRow && (curRow.previousElementSibling !== undefined || curRow.nextElementSibling !== undefined)) {
      let el = curRow.previousElementSibling;
      while (el) {
        if (el.classList && el.classList.contains && el.classList.contains('diff-row')) {
          const pl = parseInt((el.getAttribute && el.getAttribute(attr)) || '0', 10);
          if (pl > 0) return pl + 0.5;
        } else if (el.classList && el.classList.contains && el.classList.contains('row-skip')) {
          const skipEndAttr = attr === 'data-line-l' ? 'data-left-end' : 'data-right-end';
          const se = parseInt((el.getAttribute && el.getAttribute(skipEndAttr)) || '0', 10);
          if (se > 0) return se + 0.5;
        }
        el = el.previousElementSibling;
      }
      el = curRow.nextElementSibling;
      while (el) {
        if (el.classList && el.classList.contains && el.classList.contains('diff-row')) {
          const nl = parseInt((el.getAttribute && el.getAttribute(attr)) || '0', 10);
          if (nl > 0) return nl - 0.5;
        } else if (el.classList && el.classList.contains && el.classList.contains('row-skip')) {
          const skipStartAttr = attr === 'data-line-l' ? 'data-left-start' : 'data-right-start';
          const ss = parseInt((el.getAttribute && el.getAttribute(skipStartAttr)) || '0', 10);
          if (ss > 0) return ss - 0.5;
        }
        el = el.nextElementSibling;
      }
      return 0;
    }

    let prevLine = 0;
    for (let i = startIdx - 1; i >= 0; i--) {
      const pl = parseInt((rows[i] && rows[i].getAttribute ? rows[i].getAttribute(attr) : '0') || '0', 10);
      if (pl > 0) {
        prevLine = pl;
        break;
      }
    }
    if (prevLine > 0) {
      return prevLine + 0.5;
    }

    let nextLine = 0;
    for (let i = startIdx + 1; i < rows.length; i++) {
      const nl = parseInt((rows[i] && rows[i].getAttribute ? rows[i].getAttribute(attr) : '0') || '0', 10);
      if (nl > 0) {
        nextLine = nl;
        break;
      }
    }
    if (nextLine > 0) {
      return nextLine - 0.5;
    }

    return 0;
  }

  function resolveVisibleScopeOnSide(container, table, diffRowsList, rowIdx, barBottom, isScrolledUnder, side) {
    const lineAttr = side === 'L' ? 'data-line-l' : 'data-line-r';
    const lineNum = resolveSideAnchorLine(diffRowsList, rowIdx, lineAttr);
    if (lineNum <= 0) return null;

    const scopes = getContainerScopesForSide(container, side);
    const s = findScopeAtLine(scopes, lineNum);
    if (!s) return null;

    if (table && table.querySelector) {
      const declRow = table.querySelector(`tr.diff-row[${lineAttr}="${s.s}"]`);
      if (declRow && declRow.getBoundingClientRect) {
        const declRect = declRow.getBoundingClientRect();
        const winH = (typeof window !== 'undefined' && window.innerHeight) ? window.innerHeight : 1000;
        const isVisibleBelowHeader = declRect.bottom > barBottom + 4 && declRect.top < winH;
        if (isVisibleBelowHeader) {
          if (!isScrolledUnder || (declRect.top >= barBottom - 2 && declRect.top < barBottom + 30)) {
            return null;
          }
        }
      }
    }

    return s;
  }
  window.findScopeAtLine = findScopeAtLine;
  window.resolveSideAnchorLine = resolveSideAnchorLine;

  function syncAppHeaderHeight() {
    const appHeader = document.querySelector('header.app-header');
    if (!appHeader) return 0;
    const rect = appHeader.getBoundingClientRect ? appHeader.getBoundingClientRect() : null;
    const h = (rect && rect.height > 0) ? rect.height : (appHeader.offsetHeight || 0);
    if (h > 0 && document.documentElement && document.documentElement.style && document.documentElement.style.setProperty) {
      document.documentElement.style.setProperty('--app-header-h', `${h}px`);
    }
    return h;
  }
  window.syncAppHeaderHeight = syncAppHeaderHeight;

  function updateHangingFileHeaders(appBottom) {
    if (!document.querySelectorAll) return;
    const sections = document.querySelectorAll('.diff-file-section');
    if (!sections || sections.length === 0) return;

    if (appBottom === undefined) {
      const appHeader = document.querySelector('header.app-header');
      appBottom = appHeader ? Math.max(0, (appHeader.getBoundingClientRect ? appHeader.getBoundingClientRect().bottom : (appHeader.offsetHeight || 0))) : 0;
    }
    const stickyTop = appBottom > 0 ? appBottom : 42;

    const updates = [];
    for (let i = 0; i < sections.length; i++) {
      const sec = sections[i];
      const hdr = sec.querySelector ? sec.querySelector('.diff-file-header') : null;
      if (!hdr) continue;
      const isCollapsed = sec.classList && sec.classList.contains('collapsed');
      if (isCollapsed) {
        updates.push({ hdr, isHanging: false });
        continue;
      }
      const secRect = sec.getBoundingClientRect ? sec.getBoundingClientRect() : { top: 0, bottom: 0 };
      const hdrRect = hdr.getBoundingClientRect ? hdr.getBoundingClientRect() : { height: 0 };
      const hdrH = (hdrRect && hdrRect.height !== undefined && hdrRect.height > 0) ? hdrRect.height : (hdr.offsetHeight || 0);
      const isHanging = hdrH > 0 &&
        secRect.top <= stickyTop + 1 &&
        secRect.bottom > stickyTop;
      updates.push({ hdr, isHanging });
    }

    let hangingChanged = false;
    for (let i = 0; i < updates.length; i++) {
      const u = updates[i];
      if (u.hdr.classList) {
        const cur = u.hdr.classList.contains('is-hanging');
        if (cur !== u.isHanging) {
          hangingChanged = true;
          if (u.isHanging) {
            u.hdr.classList.add('is-hanging');
          } else {
            u.hdr.classList.remove('is-hanging');
          }
        }
      }
    }
    if (hangingChanged && typeof updateSnappedPopupsLayout === 'function') {
      updateSnappedPopupsLayout();
    }
  }
  window.updateHangingFileHeaders = updateHangingFileHeaders;

  function resetHiddenScopeGroup(group, bar, btnL, btnR, headerEl) {
    if (group) {
      group.style.display = 'none';
      if (group.classList) {
        group.classList.remove('is-split');
        group.classList.remove('is-empty-side');
      }
      if (group.removeAttribute) group.removeAttribute('data-active-side');
    }
    if (bar) bar.style.display = 'none';
    if (btnL) {
      btnL.style.display = 'none';
      btnL.tabIndex = -1;
      if (btnL.setAttribute) btnL.setAttribute('aria-hidden', 'true');
    }
    if (btnR) {
      btnR.style.display = 'none';
      btnR.tabIndex = -1;
      if (btnR.setAttribute) btnR.setAttribute('aria-hidden', 'true');
    }
    if (headerEl && headerEl.classList) headerEl.classList.remove('has-scope');
  }

  function updateStickyScopes() {
    syncAppHeaderHeight();
    const appHeader = document.querySelector('header.app-header');
    const appBottom = appHeader ? Math.max(0, (appHeader.getBoundingClientRect ? appHeader.getBoundingClientRect().bottom : (appHeader.offsetHeight || 0))) : 0;
    updateHangingFileHeaders(appBottom);

    const singleGroup = document.getElementById('appHeaderScopeGroup');
    const singleBar = document.getElementById('appHeaderScope');
    const singleBtnL = document.getElementById('appHeaderScopeSideLeft') || (singleGroup ? singleGroup.querySelector('.diff-scope-side-left') : null);
    const singleBtnR = document.getElementById('appHeaderScopeSideRight') || (singleGroup ? singleGroup.querySelector('.diff-scope-side-right') : null);
    resetHiddenScopeGroup(singleGroup, singleBar, singleBtnL, singleBtnR, appHeader);

    const containers = document.querySelectorAll('.diff-container[data-scopes]');
    const actions = [];

    for (const container of containers) {
      const fileSection = container.closest ? container.closest('.diff-file-section') : null;
      const fileHeader = fileSection ? (fileSection.querySelector ? fileSection.querySelector('.diff-file-header') : null) : null;
      const headerEl = fileHeader || appHeader;
      const fileGroup = fileSection ? (fileSection.querySelector ? fileSection.querySelector('.diff-header-scope-group') : null) : singleGroup;
      const fileBar = fileSection ? (fileSection.querySelector ? fileSection.querySelector('.diff-header-scope') : null) : singleBar;
      const fileBtnL = fileGroup ? (fileGroup.querySelector ? fileGroup.querySelector('.diff-scope-side-left') : null) : singleBtnL;
      const fileBtnR = fileGroup ? (fileGroup.querySelector ? fileGroup.querySelector('.diff-scope-side-right') : null) : singleBtnR;

      if (fileSection && fileSection.classList && fileSection.classList.contains('collapsed')) {
        actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
        continue;
      }
      if (container.offsetParent === null) {
        actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
        continue;
      }

      const table = container.querySelector ? container.querySelector('.diff-table') : null;
      if (!table) continue;

      let barBottom = appBottom;
      if (fileSection && fileHeader && fileHeader.getBoundingClientRect) {
        barBottom = fileHeader.getBoundingClientRect().bottom;
      }
      if (!fileBar && !fileGroup) continue;

      const tRect = table.getBoundingClientRect ? table.getBoundingClientRect() : { top: 0, bottom: 0 };
      if (tRect.bottom <= barBottom + 12 || (fileSection && window.innerHeight > 0 && tRect.top >= window.innerHeight)) {
        actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
        continue;
      }

      if (!table._diffRows) {
        table._diffRows = Array.from(table.querySelectorAll('tr.diff-row'));
      }
      const diffRowsList = table._diffRows;
      if (!diffRowsList || diffRowsList.length === 0) {
        actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
        continue;
      }

      let rowIdx = 0;
      const isScrolledUnder = tRect.top < barBottom - 2;
      if (isScrolledUnder) {
        const probeY = barBottom + 6;
        let low = 0;
        let high = diffRowsList.length - 1;
        let foundIdx = -1;
        while (low <= high) {
          const mid = (low + high) >> 1;
          const rRect = diffRowsList[mid].getBoundingClientRect ? diffRowsList[mid].getBoundingClientRect() : { bottom: 0 };
          if (rRect.bottom >= probeY) {
            foundIdx = mid;
            high = mid - 1;
          } else {
            low = mid + 1;
          }
        }
        if (foundIdx < 0) {
          actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
          continue;
        }
        rowIdx = foundIdx;
      } else {
        rowIdx = 0;
      }

      const isSplit = isSplitScopeContainer(container, table);
      if (isSplit) {
        const activeSide = getActiveContainerScopeSide(container, table);
        const otherSide = activeSide === 'L' ? 'R' : 'L';
        const sActive = resolveVisibleScopeOnSide(container, table, diffRowsList, rowIdx, barBottom, isScrolledUnder, activeSide);
        const sOther = resolveVisibleScopeOnSide(container, table, diffRowsList, rowIdx, barBottom, isScrolledUnder, otherSide);

        if (!sActive && !sOther) {
          actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
          continue;
        }

        actions.push({
          hide: false,
          isSplit: true,
          group: fileGroup,
          bar: fileBar,
          btnL: fileBtnL,
          btnR: fileBtnR,
          headerEl,
          container,
          activeSide,
          sActive,
          sOther,
        });
      } else {
        const activeSide = getActiveContainerScopeSide(container, table);
        const s = resolveVisibleScopeOnSide(container, table, diffRowsList, rowIdx, barBottom, isScrolledUnder, activeSide);
        if (!s) {
          actions.push({ hide: true, group: fileGroup, bar: fileBar, btnL: fileBtnL, btnR: fileBtnR, headerEl });
          continue;
        }
        actions.push({
          hide: false,
          isSplit: false,
          group: fileGroup,
          bar: fileBar,
          btnL: fileBtnL,
          btnR: fileBtnR,
          headerEl,
          container,
          activeSide,
          s,
        });
      }
    }

    const shownBars = [];
    for (const act of actions) {
      if (act.hide) {
        resetHiddenScopeGroup(act.group, act.bar, act.btnL, act.btnR, act.headerEl);
      } else if (act.isSplit) {
        const { group, bar, btnL, btnR, headerEl, container, activeSide, sActive, sOther } = act;
        if (group) {
          group.style.display = 'inline-flex';
          if (group.classList) group.classList.add('is-split');
          if (group.setAttribute) group.setAttribute('data-active-side', activeSide);
        }
        if (headerEl && headerEl.classList) headerEl.classList.add('has-scope');

        if (btnL) {
          btnL.style.display = '';
          btnL.tabIndex = activeSide === 'L' ? 0 : -1;
          if (activeSide === 'L') {
            if (btnL.removeAttribute) btnL.removeAttribute('aria-hidden');
          } else {
            if (btnL.setAttribute) btnL.setAttribute('aria-hidden', 'true');
          }
          btnL._scopeContainer = container;
          if (btnL.setAttribute) btnL.setAttribute('data-scope-side', 'L');
          btnL.title = (activeSide === 'L' && !sActive && sOther)
            ? 'No enclosing declaration on left diff — click for right diff'
            : 'Based on left diff — click to base on right diff';
        }
        if (btnR) {
          btnR.style.display = '';
          btnR.tabIndex = activeSide === 'R' ? 0 : -1;
          if (activeSide === 'R') {
            if (btnR.removeAttribute) btnR.removeAttribute('aria-hidden');
          } else {
            if (btnR.setAttribute) btnR.setAttribute('aria-hidden', 'true');
          }
          btnR._scopeContainer = container;
          if (btnR.setAttribute) btnR.setAttribute('data-scope-side', 'R');
          btnR.title = (activeSide === 'R' && !sActive && sOther)
            ? 'No enclosing declaration on right diff — click for left diff'
            : 'Based on right diff — click to base on left diff';
        }

        if (!sActive && sOther) {
          if (group && group.classList) {
            group.classList.add('is-empty-side');
          }
          if (bar) bar.style.display = 'none';
        } else {
          if (group && group.classList) {
            group.classList.remove('is-empty-side');
          }
          if (sActive && bar) {
            bar.style.display = 'inline-block';
            bar.textContent = sActive.h;
            bar.title = `${sActive.h} (${activeSide === 'L' ? 'left' : 'right'} L${sActive.s})`;
            bar.setAttribute('data-decl-line', String(sActive.s));
            bar.setAttribute('data-active-side', activeSide);
            if (sActive.nl > 0) {
              bar.setAttribute('data-name-line', String(sActive.nl));
              bar.setAttribute('data-name-col', String(sActive.nc || 0));
            } else if (bar.removeAttribute) {
              bar.removeAttribute('data-name-line');
              bar.removeAttribute('data-name-col');
            }
            bar._scopeContainer = container;
            shownBars.push({ bar, group, btnL, btnR, headerEl });
          } else if (bar) {
            bar.style.display = 'none';
          }
        }
      } else {
        const { group, bar, btnL, btnR, headerEl, container, activeSide, s } = act;
        if (btnL) {
          btnL.style.display = 'none';
          btnL.tabIndex = -1;
          if (btnL.setAttribute) btnL.setAttribute('aria-hidden', 'true');
        }
        if (btnR) {
          btnR.style.display = 'none';
          btnR.tabIndex = -1;
          if (btnR.setAttribute) btnR.setAttribute('aria-hidden', 'true');
        }
        if (group) {
          group.style.display = 'inline-flex';
          if (group.classList) {
            group.classList.remove('is-split');
            group.classList.remove('is-empty-side');
          }
          if (group.removeAttribute) group.removeAttribute('data-active-side');
        }
        if (headerEl && headerEl.classList) headerEl.classList.add('has-scope');

        if (bar) {
          bar.style.display = 'inline-block';
          bar.textContent = s.h;
          bar.title = `${s.h} (L${s.s})`;
          bar.setAttribute('data-decl-line', String(s.s));
          bar.setAttribute('data-active-side', activeSide);
          if (s.nl > 0) {
            bar.setAttribute('data-name-line', String(s.nl));
            bar.setAttribute('data-name-col', String(s.nc || 0));
          } else if (bar.removeAttribute) {
            bar.removeAttribute('data-name-line');
            bar.removeAttribute('data-name-col');
          }
          bar._scopeContainer = container;
          shownBars.push({ bar, group, btnL, btnR, headerEl });
        }
      }
    }

    for (const item of shownBars) {
      if (item.bar && item.bar.offsetWidth > 0 && item.bar.offsetWidth < 72) {
        const isTruncated = (typeof item.bar.scrollWidth === 'number' && typeof item.bar.clientWidth === 'number' && item.bar.clientWidth > 0) ? (item.bar.scrollWidth > item.bar.clientWidth + 1) : true;
        if (isTruncated) {
          resetHiddenScopeGroup(item.group, item.bar, item.btnL, item.btnR, item.headerEl);
        }
      }
    }
  }
  window.updateStickyScopes = updateStickyScopes;

  window.handleToggleStickyScopeSide = function(e, btnEl) {
    if (e) {
      if (e.preventDefault) e.preventDefault();
      if (e.stopPropagation) e.stopPropagation();
    }
    const wasFocused = (typeof document !== 'undefined' && document.activeElement === btnEl);
    const group = btnEl && btnEl.closest ? btnEl.closest('.diff-header-scope-group') : null;
    const nextSide = getGlobalScopeSide() === 'R' ? 'L' : 'R';
    setGlobalScopeSide(nextSide);
    if (wasFocused && group && group.querySelector) {
      const nextBtn = group.querySelector(nextSide === 'L' ? '.diff-scope-side-left' : '.diff-scope-side-right');
      if (nextBtn && (!nextBtn.style || nextBtn.style.display !== 'none') && typeof nextBtn.focus === 'function') {
        try {
          nextBtn.focus({ preventScroll: true });
        } catch (err) {
          nextBtn.focus();
        }
      }
    }
  };

  window.handleStickyScopeClick = async function(e, barEl) {
    if (e) {
      if (e.preventDefault) e.preventDefault();
      if (e.stopPropagation) e.stopPropagation();
    }
    if (!barEl) return;
    const container = barEl._scopeContainer || (barEl.closest && barEl.closest('.diff-container')) || (barEl.closest && barEl.closest('.diff-file-section') && barEl.closest('.diff-file-section').querySelector('.diff-container')) || document.querySelector('.diff-container');
    if (!container) return;
    const table = container.querySelector ? container.querySelector('.diff-table') : null;
    const side = (barEl.getAttribute && barEl.getAttribute('data-active-side')) || getActiveContainerScopeSide(container, table);
    const filePath = (container.getAttribute && container.getAttribute('data-file-path')) || '';
    const extPkg = (container.getAttribute && container.getAttribute('data-ext-pkg')) || '';
    const extFile = (container.getAttribute && container.getAttribute('data-ext-file')) || '';
    const isGo = (extPkg && extFile) ? extFile.endsWith('.go') : filePath.endsWith('.go');
    const nameLine = parseInt((barEl.getAttribute && barEl.getAttribute('data-name-line')) || '0', 10);
    const nameCol = parseInt((barEl.getAttribute && barEl.getAttribute('data-name-col')) || '0', 10);
    if (isGo && nameLine > 0) {
      const opener = (typeof window !== 'undefined' && window.openSymbolPopupAt) ? window.openSymbolPopupAt : openSymbolPopupAt;
      const opened = await opener(container, side, nameLine, nameCol, barEl, false);
      if (opened) return;
    }

    const declLine = parseInt((barEl.getAttribute && barEl.getAttribute('data-decl-line')) || '0', 10);
    if (declLine <= 0) return;
    const lineAttr = side === 'L' ? 'data-line-l' : 'data-line-r';

    if (!table) return;

    let targetTr = table.querySelector(`tr.diff-row[${lineAttr}="${declLine}"]`);
    if (!targetTr) {
      const skipRows = table.querySelectorAll('tr.row-skip');
      for (const sr of skipRows) {
        const start = parseInt(sr.getAttribute(side === 'L' ? 'data-left-start' : 'data-right-start') || '0', 10);
        const end = parseInt(sr.getAttribute(side === 'L' ? 'data-left-end' : 'data-right-end') || '0', 10);
        if (start > 0 && end > 0 && declLine >= start && declLine <= end) {
          const expandBtn = sr.querySelector('.skip-btn.skip-all');
          if (expandBtn && window.expandContextAll) {
            await window.expandContextAll(expandBtn);
          }
          break;
        }
      }
      targetTr = table.querySelector(`tr.diff-row[${lineAttr}="${declLine}"]`);
    }

    if (targetTr) {
      targetTr.scrollIntoView({ block: 'center' });
      flashDiffRows(targetTr, side === 'L' ? 'L' : 'R');
      scheduleStickyScopeUpdate();
    }
  };

  // Incremental context expansion: expandContextStep and expandContextAll
  window.expandContextAll = async function(btn) {
    const skipRow = btn.closest('tr.row-skip');
    if (!skipRow) return;
    const container = skipRow.closest('.diff-container');

    const lStartAttr = skipRow.getAttribute('data-left-start');
    const rStartAttr = skipRow.getAttribute('data-right-start');

    if (container && (lStartAttr !== null || rStartAttr !== null)) {
      let lStart = parseInt(lStartAttr || '0', 10);
      let lEnd = parseInt(skipRow.getAttribute('data-left-end') || '0', 10);
      let rStart = parseInt(rStartAttr || '0', 10);
      let rEnd = parseInt(skipRow.getAttribute('data-right-end') || '0', 10);

      const total = Math.max(
        (lEnd > 0 && lStart > 0) ? (lEnd - lStart + 1) : 0,
        (rEnd > 0 && rStart > 0) ? (rEnd - rStart + 1) : 0
      );
      if (total <= 0) {
        skipRow.remove();
        return;
      }

      const params = new URLSearchParams({
        repo: container.getAttribute('data-repo') || '',
        change: container.getAttribute('data-change') || '',
        left_rev: container.getAttribute('data-left-rev') || '',
        right_rev: container.getAttribute('data-right-rev') || '',
        path: container.getAttribute('data-file-path') || '',
        left_start: lStart,
        left_end: lEnd,
        right_start: rStart,
        right_end: rEnd,
        mode: container.getAttribute('data-diff-mode') || 'split'
      });

      btn.disabled = true;
      try {
        const res = await fetch('/api/diff/expand?' + params.toString());
        if (!res.ok) return;
        const html = await res.text();
        const tmp = document.createElement('tbody');
        tmp.innerHTML = html;
        const trs = Array.from(tmp.children);
        const table = container.querySelector('.diff-table');
        if (table) table._diffRows = null;
        if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
        skipRow.replaceWith(...trs);
        if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
        scheduleStickyScopeUpdate();
        if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
      } finally {
        btn.disabled = false;
      }
    }
  };

  window.expandContextStep = async function(btn, dir, count) {
    const skipRow = btn.closest('tr.row-skip');
    if (!skipRow) return;
    const container = skipRow.closest('.diff-container');

    const lStartAttr = skipRow.getAttribute('data-left-start');
    const rStartAttr = skipRow.getAttribute('data-right-start');

    if (container && (lStartAttr !== null || rStartAttr !== null)) {
      let lStart = parseInt(lStartAttr || '0', 10);
      let lEnd = parseInt(skipRow.getAttribute('data-left-end') || '0', 10);
      let rStart = parseInt(rStartAttr || '0', 10);
      let rEnd = parseInt(skipRow.getAttribute('data-right-end') || '0', 10);

      const total = Math.max(
        (lEnd > 0 && lStart > 0) ? (lEnd - lStart + 1) : 0,
        (rEnd > 0 && rStart > 0) ? (rEnd - rStart + 1) : 0
      );
      if (total <= 0) {
        skipRow.remove();
        return;
      }

      const step = Math.min(count, total);
      let reqLStart, reqLEnd, reqRStart, reqREnd;

      if (dir === 'down') {
        reqLStart = lStart > 0 ? lStart : 0;
        reqLEnd = lStart > 0 ? lStart + step - 1 : 0;
        reqRStart = rStart > 0 ? rStart : 0;
        reqREnd = rStart > 0 ? rStart + step - 1 : 0;
      } else {
        reqLStart = lEnd > 0 ? lEnd - step + 1 : 0;
        reqLEnd = lEnd > 0 ? lEnd : 0;
        reqRStart = rEnd > 0 ? rEnd - step + 1 : 0;
        reqREnd = rEnd > 0 ? rEnd : 0;
      }

      const params = new URLSearchParams({
        repo: container.getAttribute('data-repo') || '',
        change: container.getAttribute('data-change') || '',
        left_rev: container.getAttribute('data-left-rev') || '',
        right_rev: container.getAttribute('data-right-rev') || '',
        path: container.getAttribute('data-file-path') || '',
        left_start: reqLStart,
        left_end: reqLEnd,
        right_start: reqRStart,
        right_end: reqREnd,
        mode: container.getAttribute('data-diff-mode') || 'split'
      });

      btn.disabled = true;
      try {
        const res = await fetch('/api/diff/expand?' + params.toString());
        if (!res.ok) return;
        const html = await res.text();
        const tmp = document.createElement('tbody');
        tmp.innerHTML = html;
        const trs = Array.from(tmp.children);

        const table = container.querySelector('.diff-table');
        if (table) table._diffRows = null;
        if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();

        if (dir === 'down') {
          skipRow.before(...trs);
          if (lStart > 0) lStart += step;
          if (rStart > 0) rStart += step;
          skipRow.setAttribute('data-left-start', lStart);
          skipRow.setAttribute('data-right-start', rStart);
        } else {
          skipRow.after(...trs);
          if (lEnd > 0) lEnd -= step;
          if (rEnd > 0) rEnd -= step;
          skipRow.setAttribute('data-left-end', lEnd);
          skipRow.setAttribute('data-right-end', rEnd);
        }

        const rem = Math.max(
          (lEnd > 0 && lStart > 0) ? (lEnd - lStart + 1) : 0,
          (rEnd > 0 && rStart > 0) ? (rEnd - rStart + 1) : 0
        );
        if (rem <= 0) {
          skipRow.remove();
        } else {
          const countSpan = skipRow.querySelector('.skip-count');
          if (countSpan) countSpan.textContent = rem;
          const skipAllBtn = skipRow.querySelector('.skip-all');
          if (skipAllBtn) skipAllBtn.title = `Expand all ${rem} common lines`;
          if (rem <= 10) {
            skipRow.querySelector('.skip-tabs')?.remove();
            skipRow.classList.add('skip-no-tabs');
          }
        }
        scheduleStickyScopeUpdate();
        if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
      } finally {
        btn.disabled = false;
      }
    }
  };

  window.replyThread = function(e, threadID) {
    if (e && e.preventDefault) e.preventDefault();
    const replyWrap = document.getElementById('reply-form-' + threadID);
    const form = (e && e.target && e.target.tagName === 'FORM' && e.target.querySelector)
      ? e.target
      : ((replyWrap && replyWrap.querySelector && replyWrap.querySelector('form')) ||
         (e && e.target && e.target.closest && e.target.closest('form')) ||
         (e && e.target && e.target.querySelector ? e.target : replyWrap));
    if (!form) return;
    const ta = form.querySelector('textarea');
    if (!ta || !ta.value.trim()) return;
    if (form._submitting) return;
    form._submitting = true;

    const resCb = form.querySelector('input[name="resolved"]');
    const resolved = resCb ? !!resCb.checked : false;

    const container = form.closest ? form.closest('.diff-container') : null;
    const leftRev = container ? (container.getAttribute('data-left-rev') || '') : '';
    const rightRev = container ? (container.getAttribute('data-right-rev') || '') : '';

    const replyKey = (replyWrap && replyWrap.getAttribute('data-draft-key')) ||
      (form.getAttribute && form.getAttribute('data-draft-key')) ||
      (typeof getReplyDraftKey === 'function' ? getReplyDraftKey(threadID, form) : null);
    const replyFbKey = `reply:${threadID}`;
    for (const k of [replyKey, replyFbKey]) {
      if (k && draftSaveTimers.has(k)) {
        clearTimeout(draftSaveTimers.get(k));
        draftSaveTimers.delete(k);
      }
    }

    beginSelfReload();
    fetch('/api/threads/reply', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        thread_id: threadID,
        body: ta.value.trim(),
        resolved: resolved,
        left_rev: leftRev,
        right_rev: rightRev
      })
    }).then(res => {
      if (res.ok) return res.json();
      cancelSelfReload();
      throw new Error('failed');
    }).then(data => {
      if (!data) return;
      if (data.sig) pageStateSig = data.sig;
      suppressSSEReloadUntil = Date.now() + 300;
      clearRangeHighlights();
      try {
        if (replyKey) {
          deleteFormDraftServer(replyKey, { thread_id: threadID });
        }
        formDraftsMap.delete(replyFbKey);
      } catch (_) {}
      const card = document.getElementById('thread-' + threadID);
      const nextUnresolved = (data.thread && typeof data.thread.resolved === 'boolean') ? !data.thread.resolved : !resolved;
      syncThreadDraftsUnresolved(threadID, nextUnresolved, card, data.thread);
      if (card && data.html) {
        replaceThreadCardHTML(card, data.html, { preserveCollapsed: true });
      }
      cleanupCommentZIndicesIfNeeded();
      updateCommentCountBadges(data.file_path, data.file_counts, data.change_counts);
    }).catch(() => {
      cancelSelfReload();
    }).finally(() => {
      form._submitting = false;
    });
  };

  window.resolveThread = function(threadID, resolved) {
    const card = document.getElementById('thread-' + threadID);
    const container = card ? card.closest('.diff-container') : null;
    const leftRev = container ? (container.getAttribute('data-left-rev') || '') : '';
    const rightRev = container ? (container.getAttribute('data-right-rev') || '') : '';

    beginSelfReload();
    fetch('/api/threads/resolve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        thread_id: threadID,
        resolved: resolved,
        left_rev: leftRev,
        right_rev: rightRev
      })
    }).then(res => {
      if (res.ok) return res.json();
      cancelSelfReload();
      throw new Error('failed');
    }).then(data => {
      if (!data) return;
      if (data.sig) pageStateSig = data.sig;
      suppressSSEReloadUntil = Date.now() + 300;
      clearRangeHighlights();

      const nextUnresolved = (data.thread && typeof data.thread.resolved === 'boolean') ? !data.thread.resolved : !resolved;
      syncThreadDraftsUnresolved(threadID, nextUnresolved, card, data.thread);

      if (card && data.html) {
        replaceThreadCardHTML(card, data.html);
      }
      cleanupCommentZIndicesIfNeeded();
      updateCommentCountBadges(data.file_path, data.file_counts, data.change_counts);
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.deleteMessage = function(msgID) {
    const msgEl = document.getElementById('msg-' + msgID) || document.getElementById('msg-edit-' + msgID);
    const draftKey = getEditDraftKey(msgID, msgEl);
    deleteFormDraftServer(draftKey, { message_id: msgID });
    if (draftSaveTimers.has(`edit:${msgID}`)) {
      clearTimeout(draftSaveTimers.get(`edit:${msgID}`));
      draftSaveTimers.delete(`edit:${msgID}`);
    }
    formDraftsMap.delete(`edit:${msgID}`);
    const container = msgEl ? msgEl.closest('.diff-container') : null;
    const leftRev = container ? (container.getAttribute('data-left-rev') || '') : '';
    const rightRev = container ? (container.getAttribute('data-right-rev') || '') : '';

    beginSelfReload();
    fetch('/api/threads/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        message_id: msgID,
        left_rev: leftRev,
        right_rev: rightRev
      })
    }).then(res => {
      if (res.ok) return res.json();
      cancelSelfReload();
      throw new Error('failed');
    }).then(data => {
      if (!data) return;
      if (data.sig) pageStateSig = data.sig;
      suppressSSEReloadUntil = Date.now() + 300;
      clearRangeHighlights();
      if (data.thread_deleted) {
        try {
          const root = container || document;
          const info = getCurrentRepoAndChange(root);
          if (info.repo && info.change) {
            const replyK = `review_draft:reply:${info.repo}:${info.change}:${data.thread_id}`;
            deleteFormDraftServer(replyK, { thread_id: data.thread_id });
          }
          if (draftSaveTimers.has(`reply:${data.thread_id}`)) {
            clearTimeout(draftSaveTimers.get(`reply:${data.thread_id}`));
            draftSaveTimers.delete(`reply:${data.thread_id}`);
          }
          formDraftsMap.delete(`reply:${data.thread_id}`);
        } catch (_) {}
        const card = document.getElementById('thread-' + data.thread_id);
        if (card) {
          const row = card.closest('tr.thread-row');
          card.remove();
          if (row && !row.querySelector('.thread-card')) {
            row.remove();
          }
          refreshLayoutAfterCollapseChange();
        }
      } else if (data.html) {
        const card = document.getElementById('thread-' + data.thread_id);
        if (card) {
          replaceThreadCardHTML(card, data.html, { preserveCollapsed: true });
        }
      }
      cleanupCommentZIndicesIfNeeded();
      updateCommentCountBadges(data.file_path, data.file_counts, data.change_counts);
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.toggleFileReviewed = function(repo, change, rev, path, reviewed) {
    const btn = document.getElementById('toggleReviewedBtn');
    const nextVal = btn ? !btn.classList.contains('btn-active') : !!reviewed;
    beginSelfReload();
    fetch('/api/files/reviewed', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        repo: repo,
        change: change,
        rev: rev,
        path: path,
        reviewed: nextVal
      })
    }).then(async res => {
      if (res.ok) {
        try {
          const data = await res.json();
          if (data && data.sig) {
            pageStateSig = data.sig;
          }
        } catch (_) {}
        suppressSSEReloadUntil = Date.now() + 300;
        if (btn) {
          if (nextVal) {
            btn.classList.add('btn-active');
            btn.textContent = 'Reviewed ✓';
          } else {
            btn.classList.remove('btn-active');
            btn.textContent = 'Mark Reviewed (m)';
          }
        }
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.toggleChangeRevMark = function(repo, change, rev, kind, val) {
    const lgtmBtn = document.getElementById('lgtmBtn');
    let nextVal = val;
    if (kind === 'lgtm' && lgtmBtn) {
      nextVal = !lgtmBtn.classList.contains('btn-blue');
    }
    const payload = { repo, change, rev };
    payload[kind] = nextVal;
    beginSelfReload();
    fetch('/api/changes/reviewed', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(async res => {
      if (res.ok) {
        let data = null;
        try {
          data = await res.json();
          if (data && data.sig) {
            pageStateSig = data.sig;
          }
        } catch (_) {}
        suppressSSEReloadUntil = Date.now() + 300;
        if (kind === 'lgtm') {
          const isLGTM = (data && typeof data.lgtm === 'boolean') ? data.lgtm : nextVal;
          if (lgtmBtn) {
            if (isLGTM) {
              lgtmBtn.className = 'btn btn-sm btn-blue';
              lgtmBtn.textContent = 'Revert LGTM';
            } else {
              lgtmBtn.className = 'btn btn-sm btn-green';
              lgtmBtn.textContent = 'LGTM';
            }
          }
          const appHeader = document.querySelector('header.app-header');
          if (appHeader && (appHeader.classList.contains('header-status-lgtm') || appHeader.classList.contains('header-status-active'))) {
            if (isLGTM) {
              appHeader.classList.remove('header-status-active');
              appHeader.classList.add('header-status-lgtm');
            } else {
              appHeader.classList.remove('header-status-lgtm');
              appHeader.classList.add('header-status-active');
            }
            if (typeof syncAppHeaderHeight === 'function') {
              syncAppHeaderHeight();
            }
          }
          const commitBadge = document.getElementById('commitStatusBadge');
          if (commitBadge) {
            if (isLGTM) {
              commitBadge.className = 'badge badge-status-lgtm';
              commitBadge.textContent = 'LGTM';
            } else {
              commitBadge.className = 'badge badge-status-active';
              commitBadge.textContent = 'Active';
            }
          }
          const relRowBadge = document.querySelector('.relation-chain-card li.current-change-row .commit-info .badge-status-lgtm, .relation-chain-card li.current-change-row .commit-info .badge-status-active');
          if (relRowBadge) {
            if (isLGTM) {
              relRowBadge.className = 'badge badge-status-lgtm';
              relRowBadge.textContent = 'LGTM';
            } else {
              relRowBadge.className = 'badge badge-status-active';
              relRowBadge.textContent = 'Active';
            }
          }
          if (isLGTM) {
            document.querySelectorAll('button[data-reviewed-btn]').forEach(b => {
              const pathAttr = b.getAttribute('data-reviewed-btn');
              if (pathAttr) {
                b.classList.add('btn-active');
                if (b.classList.contains('file-mark-reviewed-btn')) {
                  b.textContent = 'Mark Unreviewed';
                } else {
                  b.textContent = 'Reviewed ✓';
                }
              }
            });
            document.querySelectorAll('[data-reviewed-icon]').forEach(icon => {
              const pathAttr = icon.getAttribute('data-reviewed-icon');
              if (pathAttr) {
                icon.style.display = 'inline-flex';
              }
            });
          }
        }
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.publishDrafts = function(repo, change) {
    const el = document.querySelector('[data-left-rev][data-right-rev]');
    const leftRev = el ? el.getAttribute('data-left-rev') : '';
    const rightRev = el ? el.getAttribute('data-right-rev') : '';
    beginSelfReload();
    fetch('/api/publish', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repo, change, left_rev: leftRev, right_rev: rightRev })
    }).then(async res => {
      if (res.ok) {
        let data = null;
        try {
          data = await res.json();
          if (data && data.sig) {
            pageStateSig = data.sig;
          }
        } catch (_) {}
        suppressSSEReloadUntil = Date.now() + 300;
        if (!data || !data.published) {
          cancelSelfReload();
          return;
        }
        for (const [k, timer] of Array.from(draftSaveTimers.entries())) {
          const d = formDraftsMap.get(k);
          if ((d && d.message_id) || k.startsWith('review_draft:edit:') || k.startsWith('edit:')) {
            clearTimeout(timer);
            draftSaveTimers.delete(k);
          }
        }
        for (const [k, d] of Array.from(formDraftsMap.entries())) {
          if ((d && d.message_id) || k.startsWith('review_draft:edit:') || k.startsWith('edit:')) {
            formDraftsMap.delete(k);
          }
        }
        if (!change) {
          window.location.reload();
          return;
        }
        const targetSel = document.querySelector ? document.querySelector('#diffForm select[name="target"]') : null;
        if (targetSel && targetSel.value === 'live' && data.live_rev && rightRev && data.live_rev !== rightRev) {
          window.location.reload();
          return;
        }
        let domChanged = false;
        if (data.thread_html) {
          for (const [tid, html] of Object.entries(data.thread_html)) {
            const thCard = document.getElementById('thread-' + tid);
            if (thCard) {
              replaceThreadCardHTML(thCard, html, { batch: true, preservePublishedCollapsed: true });
              domChanged = true;
            }
          }
        }
        if (Array.isArray(data.hidden_thread_ids)) {
          for (const tid of data.hidden_thread_ids) {
            const thCard = document.getElementById('thread-' + tid);
            if (thCard) {
              const row = thCard.closest ? thCard.closest('tr.thread-row') : null;
              const fileWrap = thCard.closest ? thCard.closest('.file-threads-container') : null;
              if (thCard.remove) {
                thCard.remove();
              } else if (thCard.parentNode && thCard.parentNode.removeChild) {
                thCard.parentNode.removeChild(thCard);
              }
              if (row && row.querySelector && !row.querySelector('.thread-card')) {
                if (row.remove) {
                  row.remove();
                } else if (row.parentNode && row.parentNode.removeChild) {
                  row.parentNode.removeChild(row);
                }
              }
              if (fileWrap && fileWrap.querySelector && !fileWrap.querySelector('.thread-card')) {
                if (fileWrap.remove) {
                  fileWrap.remove();
                } else if (fileWrap.parentNode && fileWrap.parentNode.removeChild) {
                  fileWrap.parentNode.removeChild(fileWrap);
                }
              }
              domChanged = true;
            }
          }
        }
        if (domChanged) {
          refreshLayoutAfterCollapseChange();
        }
        cleanupCommentZIndicesIfNeeded();
        if (data.change_counts) {
          updateCommentCountBadges(null, null, data.change_counts);
        }
        if (data.file_counts) {
          for (const [fPath, fCounts] of Object.entries(data.file_counts)) {
            updateCommentCountBadges(fPath, fCounts, null);
          }
        }
        if (data.snapshot_pending === false && data.target_desc && (!targetSel || targetSel.value === 'live') && document.querySelector) {
          const pendingBadge = document.querySelector('.change-header-bar .badge-pending');
          if (pendingBadge) {
            pendingBadge.className = 'badge';
            pendingBadge.textContent = data.target_desc;
          }
        }
        if (Array.isArray(data.snapshots) && data.snapshots.length > 0 && document.querySelector && document.createElement) {
          if (targetSel && targetSel.options) {
            const liveOpt = Array.from(targetSel.options).find(o => o.value === 'live');
            if (liveOpt) {
              liveOpt.textContent = data.snapshot_pending ? 'Live Snapshot (pending)' : 'Live Snapshot';
              for (const sn of data.snapshots) {
                const val = 'snap:' + sn.num;
                if (!Array.from(targetSel.options).some(o => o.value === val)) {
                  const opt = document.createElement('option');
                  opt.value = val;
                  opt.textContent = 'Snapshot ' + sn.num;
                  if (targetSel.insertBefore) {
                    targetSel.insertBefore(opt, targetSel.options[1] || null);
                  } else if (targetSel.appendChild) {
                    targetSel.appendChild(opt);
                  }
                }
              }
            }
          }
          const baseSel = document.querySelector('#diffForm select[name="base"]');
          if (baseSel && baseSel.options) {
            for (const sn of data.snapshots) {
              const val = 'snap:' + sn.num;
              if (!Array.from(baseSel.options).some(o => o.value === val)) {
                const opt = document.createElement('option');
                opt.value = val;
                opt.textContent = 'Snapshot ' + sn.num;
                if (baseSel.appendChild) {
                  baseSel.appendChild(opt);
                }
              }
            }
          }
          if (data.target_desc && targetSel && targetSel.value === 'live') {
            const summaryHeader = document.querySelector('.file-summary-card .files-header');
            if (summaryHeader && summaryHeader.querySelectorAll) {
              summaryHeader.querySelectorAll('span').forEach(sp => {
                if (sp.textContent === 'Target: Live Snapshot') {
                  sp.textContent = 'Target: ' + data.target_desc;
                }
              });
            }
          }
        }
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.takeSnapshot = function(repo, change) {
    beginSelfReload();
    fetch('/api/snapshot', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repo, change })
    }).then(res => {
      if (res.ok) {
        window.location.reload();
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.forgetRepo = function(slug) {
    if (!confirm(`Forget repository "${slug}"? Any snapshots and review comments will be permanently removed.`)) return;
    beginSelfReload();
    fetch('/api/repos/forget', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repo: slug })
    }).then(res => {
      if (res.ok) {
        window.location.reload();
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.toggleShortcutsModal = function() {
    const modal = document.getElementById('shortcutsModal');
    if (modal) {
      modal.style.display = (modal.style.display === 'flex') ? 'none' : 'flex';
    }
  };

  function markReviewedAndNext() {
    const isAllFiles = document.querySelector('.diff-all-files') != null;
    if (isAllFiles) {
      const sections = Array.from(document.querySelectorAll('.diff-file-section'));
      if (sections.length === 0) return;
      let curSec = sections[0];
      for (const s of sections) {
        const rect = s.getBoundingClientRect();
        if (rect.top <= 120) {
          curSec = s;
        } else {
          break;
        }
      }
      const btn = curSec.querySelector('button[data-reviewed-btn]');
      if (btn && !btn.classList.contains('btn-active')) {
        btn.click();
      }
      const curIdx = sections.indexOf(curSec);
      for (let i = 1; i < sections.length; i++) {
        const s = sections[(curIdx + i) % sections.length];
        const b = s.querySelector('button[data-reviewed-btn]');
        if (b && !b.classList.contains('btn-active')) {
          s.scrollIntoView({ block: 'start', behavior: 'smooth' });
          const body = s.querySelector('.diff-file-body');
          if (body && body.style.display === 'none') {
            const collapseBtn = s.querySelector('.file-collapse-btn');
            if (collapseBtn) collapseBtn.click();
          }
          break;
        }
      }
      return;
    }

    const container = document.querySelector('.diff-container');
    if (!container) return;
    let repoSlug = container.getAttribute('data-repo') || '';
    let changeKey = container.getAttribute('data-change') || '';
    if (!repoSlug || !changeKey) {
      const parts = window.location.pathname.split('/');
      const rIdx = parts.indexOf('r');
      const cIdx = parts.indexOf('c');
      if (rIdx >= 0 && parts[rIdx + 1]) repoSlug = decodeURIComponent(parts[rIdx + 1]);
      if (cIdx >= 0 && parts[cIdx + 1]) changeKey = decodeURIComponent(parts[cIdx + 1]);
    }
    const path = container.getAttribute('data-file-path') || '';
    const rev = container.getAttribute('data-right-rev') || '';
    const nextUnreviewed = container.getAttribute('data-next-unreviewed') || '';

    beginSelfReload();
    fetch('/api/files/reviewed', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        repo: repoSlug,
        change: changeKey,
        rev: rev,
        path: path,
        reviewed: true
      })
    }).then(res => {
      if (res.ok) {
        if (nextUnreviewed) {
          const encoded = nextUnreviewed.replace(/^\/+/, '').split('/').map(encodeURIComponent).join('/');
          window.location.href = `/r/${encodeURIComponent(repoSlug)}/c/${encodeURIComponent(changeKey)}/f/${encoded}${window.location.search}`;
        } else {
          window.location.href = `/r/${encodeURIComponent(repoSlug)}/c/${encodeURIComponent(changeKey)}${window.location.search}`;
        }
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  }

  // All Diffs & File List Row Click Helpers
  function updateToggleAllFilesBtn() {
    const btn = document.getElementById('toggleAllFilesBtn');
    if (!btn) return;
    const bodies = document.querySelectorAll('.diff-file-body');
    if (bodies.length === 0) return;
    let anyCollapsed = false;
    for (const b of bodies) {
      if (b.style.display === 'none') {
        anyCollapsed = true;
        break;
      }
    }
    btn.textContent = anyCollapsed ? 'Expand All' : 'Collapse All';
  }
  window.updateToggleAllFilesBtn = updateToggleAllFilesBtn;

  window.toggleAllFiles = function() {
    const bodies = document.querySelectorAll('.diff-file-body');
    if (bodies.length === 0) return;
    let anyCollapsed = false;
    for (const b of bodies) {
      if (b.style.display === 'none') {
        anyCollapsed = true;
        break;
      }
    }
    const expand = anyCollapsed;
    const collapsedRegions = [];
    if (!expand) {
      bodies.forEach(b => {
        if (b.style.display !== 'none') {
          const sec = b.closest('.diff-file-section');
          if (sec && sec.getBoundingClientRect) {
            const rect = sec.getBoundingClientRect();
            const scrollY = window.scrollY || 0;
            collapsedRegions.push({
              secTop: rect.top + scrollY,
              oldBottom: rect.bottom + scrollY,
              secEl: sec,
            });
          }
        }
      });
    }

    bodies.forEach(b => {
      const sec = b.closest('.diff-file-section');
      const chevron = sec ? sec.querySelector('.file-collapse-btn') : null;
      if (expand) {
        b.style.display = 'block';
        if (sec) sec.classList.remove('collapsed');
        if (chevron) chevron.textContent = '▾';
      } else {
        b.style.display = 'none';
        if (sec) sec.classList.add('collapsed');
        if (chevron) chevron.textContent = '▸';
        const groupEl = sec ? sec.querySelector('.diff-header-scope-group') : null;
        const scopeEl = sec ? sec.querySelector('.diff-header-scope') : null;
        const btnL = groupEl ? groupEl.querySelector('.diff-scope-side-left') : (sec ? sec.querySelector('.diff-scope-side-left') : null);
        const btnR = groupEl ? groupEl.querySelector('.diff-scope-side-right') : (sec ? sec.querySelector('.diff-scope-side-right') : null);
        const fileHdr = sec ? sec.querySelector('.diff-file-header') : null;
        resetHiddenScopeGroup(groupEl, scopeEl, btnL, btnR, fileHdr);
      }
    });

    if (!expand && typeof relocatePopupsAfterCollapse === 'function') {
      relocatePopupsAfterCollapse(collapsedRegions);
    } else if (expand && typeof handleNaturalPageHeightChange === 'function') {
      handleNaturalPageHeightChange();
    }

    if (expand) {
      const lazyBodies = [];
      bodies.forEach(b => {
        if (b.getAttribute('data-lazy-url')) {
          lazyBodies.push(b);
        }
      });
      if (lazyBodies.length > 0) {
        let cursor = 0;
        const concurrency = 4;
        function runNext() {
          if (cursor >= lazyBodies.length) return;
          const target = lazyBodies[cursor++];
          ensureFileBodyLoaded(target).finally(() => {
            runNext();
          });
        }
        for (let w = 0; w < Math.min(concurrency, lazyBodies.length); w++) {
          runNext();
        }
      }
    }
    window.updateToggleAllFilesBtn();
    saveExpandedFiles();
    if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
    scheduleStickyScopeUpdate();
    if (typeof updateSnappedPopupsLayout === 'function') updateSnappedPopupsLayout();
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  };

  window.toggleFileSection = function(i) {
    const body = document.getElementById('file-body-' + i);
    if (!body) return;
    const sec = document.getElementById('file-sec-' + i);
    const btn = document.querySelector(`#file-sec-${i} .file-collapse-btn`);
    if (body.style.display === 'none') {
      body.style.display = 'block';
      if (sec) sec.classList.remove('collapsed');
      if (btn) btn.textContent = '▾';
      if (typeof handleNaturalPageHeightChange === 'function') {
        handleNaturalPageHeightChange();
      }
      ensureFileBodyLoaded(body);
    } else {
      const collapsedRegions = [];
      if (sec && sec.getBoundingClientRect) {
        const rect = sec.getBoundingClientRect();
        const scrollY = window.scrollY || 0;
        collapsedRegions.push({
          secTop: rect.top + scrollY,
          oldBottom: rect.bottom + scrollY,
          secEl: sec,
        });
      }
      body.style.display = 'none';
      if (sec) sec.classList.add('collapsed');
      if (btn) btn.textContent = '▸';
      const groupEl = sec ? sec.querySelector('.diff-header-scope-group') : null;
      const scopeEl = sec ? sec.querySelector('.diff-header-scope') : null;
      const btnL = groupEl ? groupEl.querySelector('.diff-scope-side-left') : (sec ? sec.querySelector('.diff-scope-side-left') : null);
      const btnR = groupEl ? groupEl.querySelector('.diff-scope-side-right') : (sec ? sec.querySelector('.diff-scope-side-right') : null);
      const fileHdr = sec ? sec.querySelector('.diff-file-header') : null;
      resetHiddenScopeGroup(groupEl, scopeEl, btnL, btnR, fileHdr);
      if (typeof relocatePopupsAfterCollapse === 'function') {
        relocatePopupsAfterCollapse(collapsedRegions);
      }
    }
    window.updateToggleAllFilesBtn();
    saveExpandedFiles();
    if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
    scheduleStickyScopeUpdate();
    if (typeof updateSnappedPopupsLayout === 'function') updateSnappedPopupsLayout();
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  };

  window.handleFileHeaderClick = function(e, i) {
    if (e.target.closest('button, a, input, select, textarea, .diff-header-scope-group')) return;
    const sel = window.getSelection();
    if (sel && !sel.isCollapsed) return;
    window.toggleFileSection(i);
  };

  window.handleFileRowClick = function(e, row) {
    if (!row || e.target.closest('button, a, input, select, textarea')) return;
    const sel = window.getSelection();
    if (sel && !sel.isCollapsed) return;
    const href = row.getAttribute('data-href');
    if (!href) return;
    if (e.metaKey || e.ctrlKey) {
      window.open(href, '_blank');
    } else {
      window.location.href = href;
    }
  };

  window.toggleFileReviewedInPlace = function(btn, repo, change, rev, path, i) {
    const isReviewed = btn.classList.contains('btn-active');
    const nextVal = !isReviewed;
    const cleanPath = (path || '').replace(/^\/+/, '');
    beginSelfReload();

    fetch('/api/files/reviewed', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        repo: repo,
        change: change,
        rev: rev,
        path: path,
        reviewed: nextVal
      })
    }).then(async res => {
      if (res.ok) {
        try {
          const data = await res.json();
          if (data && data.sig) {
            pageStateSig = data.sig;
          }
        } catch (_) {}
        suppressSSEReloadUntil = Date.now() + 300;
        document.querySelectorAll(`[data-reviewed-btn="${escapeAttrVal(cleanPath)}"]`).forEach(b => {
          if (nextVal) {
            b.classList.add('btn-active');
            if (b.classList.contains('file-mark-reviewed-btn')) {
              b.textContent = 'Mark Unreviewed';
            } else {
              b.textContent = 'Reviewed ✓';
            }
          } else {
            b.classList.remove('btn-active');
            b.textContent = 'Mark Reviewed';
          }
        });
        document.querySelectorAll(`[data-reviewed-icon="${escapeAttrVal(cleanPath)}"]`).forEach(icon => {
          icon.style.display = nextVal ? 'inline-flex' : 'none';
        });
        if (i !== undefined && i !== null) {
          document.querySelectorAll('.file-reviewed-btn-' + i).forEach(b => {
            if (nextVal) {
              b.classList.add('btn-active');
              b.textContent = 'Reviewed ✓';
            } else {
              b.classList.remove('btn-active');
              b.textContent = 'Mark Reviewed';
            }
          });
          document.querySelectorAll('.file-reviewed-icon-' + i).forEach(icon => {
            icon.style.display = nextVal ? 'inline-flex' : 'none';
          });
        }
      } else {
        cancelSelfReload();
      }
    }).catch(() => {
      cancelSelfReload();
    });
  };

  window.buildCopyPageURL = function(btn, currentHref) {
    const raw = currentHref || window.location.href;
    const copyHost = btn ? (btn.getAttribute('data-copy-host') || '').trim() : '';
    if (!copyHost) return raw;
    try {
      const u = new URL(raw);
      u.protocol = 'http:';
      u.host = copyHost;
      return u.toString();
    } catch (e) {
      return raw;
    }
  };

  window.copyPageLink = async function(btn) {
    if (!btn) return;
    const url = window.buildCopyPageURL(btn);
    let ok = false;
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(url);
        ok = true;
      } catch (_) {}
    }
    if (!ok) {
      try {
        const ta = document.createElement('textarea');
        ta.value = url;
        ta.style.position = 'fixed';
        ta.style.left = '-9999px';
        ta.style.top = '-9999px';
        document.body.appendChild(ta);
        ta.focus();
        ta.select();
        ok = document.execCommand('copy');
        document.body.removeChild(ta);
      } catch (_) {}
    }

    btn.classList.add('copied');
    btn.title = 'Copied!';
    if (btn._copyTimer) clearTimeout(btn._copyTimer);
    btn._copyTimer = setTimeout(() => {
      btn.classList.remove('copied');
      btn.title = 'Copy link to page';
    }, 1500);
  };

  // --- Go Symbol Lookup & Definition Popup ---

  function closeSpecificSymbolPopup(inst) {
    if (!inst) return false;
    hideSnapPreview();
    if (inst._cleanupActivePointer) {
      inst._cleanupActivePointer();
      inst._cleanupActivePointer = null;
    }
    if (inst._abortPendingNav) {
      try { inst._abortPendingNav(); } catch (_) {}
      inst._abortPendingNav = null;
    }
    if (inst.lookupAbort) {
      inst.lookupAbort.abort();
      inst.lookupAbort = null;
    }
    if (inst.listAbort) {
      inst.listAbort.abort();
      inst.listAbort = null;
    }

    const idx = openSymbolPopups.indexOf(inst);
    if (idx !== -1) {
      openSymbolPopups.splice(idx, 1);
    }
    if (activePopupInst === inst) {
      activePopupInst = null;
    }

    if (inst.token) {
      const hasOtherWithSameToken = openSymbolPopups.some(p => p.token === inst.token);
      if (!hasOtherWithSameToken && inst.token.classList && inst.token.classList.remove) {
        inst.token.classList.remove('sym-active');
      }
    }

    if (inst.el && inst.el.remove) {
      inst.el.remove();
    }

    if (openSymbolPopups.length === 0) {
      nextPopupZIndex = 200;
      document.querySelectorAll('.sym-active').forEach(el => {
        if (el.classList && el.classList.remove) el.classList.remove('sym-active');
      });
      cleanupCommentZIndicesIfNeeded();
    }
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();

    return true;
  }
  window.closeSpecificSymbolPopup = closeSpecificSymbolPopup;

  function resolvePopupInst(popupOrInst) {
    if (!popupOrInst) return null;
    if (popupOrInst.el) return popupOrInst;
    return openSymbolPopups.find(p => p && p.el === popupOrInst) || null;
  }

  function isInstShaded(popupOrInst) {
    const inst = resolvePopupInst(popupOrInst);
    if (inst) return !!inst.shaded;
    return !!(popupOrInst && popupOrInst.classList && popupOrInst.classList.contains('sym-shaded'));
  }

  function renderPopupWindowControlsHtml(popupOrInst) {
    const shaded = isInstShaded(popupOrInst);
    const glyph = shaded ? '+' : '−';
    const label = shaded ? 'Unshade popup' : 'Shade popup';
    return `<button type="button" class="sym-btn-shade" title="${label}" aria-label="${label}" aria-expanded="${shaded ? 'false' : 'true'}">${glyph}</button><button type="button" class="sym-btn-close" aria-label="Close">×</button>`;
  }

  function syncPopupShadeUI(popupOrInst) {
    const inst = resolvePopupInst(popupOrInst);
    const el = (inst && inst.el) || (popupOrInst && popupOrInst.el) || popupOrInst;
    if (!el) return;
    const shaded = inst ? !!inst.shaded : !!(el.classList && el.classList.contains('sym-shaded'));
    if (el.classList) {
      if (shaded && el.classList.add) el.classList.add('sym-shaded');
      else if (!shaded && el.classList.remove) el.classList.remove('sym-shaded');
    }
    const btn = el.querySelector ? el.querySelector('.sym-btn-shade') : null;
    if (btn) {
      const glyph = shaded ? '+' : '−';
      const label = shaded ? 'Unshade popup' : 'Shade popup';
      btn.textContent = glyph;
      btn.title = label;
      if (btn.setAttribute) {
        btn.setAttribute('title', label);
        btn.setAttribute('aria-label', label);
        btn.setAttribute('aria-expanded', shaded ? 'false' : 'true');
      }
    }
  }

  function setPopupShaded(inst, nextShaded, markUsed) {
    inst = resolvePopupInst(inst);
    if (!inst) return false;
    const want = !!nextShaded;
    const changed = (!!inst.shaded !== want);
    inst.shaded = want;
    if (markUsed !== false) {
      inst.usedShade = true;
    }
    syncPopupShadeUI(inst);
    if (changed && typeof updateIdentOccurrences === 'function') {
      updateIdentOccurrences();
    }
    return want;
  }
  window.setPopupShaded = setPopupShaded;

  function togglePopupShade(inst) {
    inst = resolvePopupInst(inst);
    if (!inst) return false;
    return setPopupShaded(inst, !inst.shaded, true);
  }
  window.togglePopupShade = togglePopupShade;

  function stepPopupBackOrCancel(inst) {
    if (!inst) return false;
    if (inst._abortPendingNav) {
      if (inst.shaded) setPopupShaded(inst, false, false);
      try { inst._abortPendingNav(); } catch (_) {}
      inst._abortPendingNav = null;
      return true;
    }
    if (inst.history && inst.history.length > 0) {
      return navigatePopupBack(inst);
    }
    return false;
  }

  function navigatePopupBack(inst) {
    if (!inst || !inst.history || inst.history.length === 0) return false;
    if (inst.shaded) setPopupShaded(inst, false, false);
    inst._abortPendingNav = null;
    if (inst.listAbort) {
      inst.listAbort.abort();
      inst.listAbort = null;
    }
    if (inst.lookupAbort) {
      inst.lookupAbort.abort();
      inst.lookupAbort = null;
    }
    const prevState = inst.history.pop();
    inst.currentState = prevState;
    const canBack = inst.history.length > 0;
    if (prevState.kind === 'refs') {
      renderRefsView(inst, prevState.data, prevState.ctx, prevState.scrollTop, prevState.scrollLeft);
    } else if (prevState.kind === 'impls') {
      renderImplsView(inst, prevState.data, prevState.ctx, prevState.scrollTop, prevState.scrollLeft);
    } else if (prevState.kind === 'move') {
      renderMovePopup(inst, prevState.data, prevState.ctx, prevState.scrollTop, canBack);
    } else {
      renderSymbolPopup(inst, prevState.data, prevState.ctx, prevState.scrollTop, canBack);
    }
    if (activePopupInst === inst && inst.el && inst.el.focus) {
      inst.el.focus({ preventScroll: true });
    }
    return true;
  }

  function closeSymbolPopup() {
    if (openSymbolPopups.length === 0) {
      const legacy = document.getElementById('symbolPopup');
      if (legacy && legacy.remove) {
        legacy.remove();
        return true;
      }
      return false;
    }
    const latest = openSymbolPopups[openSymbolPopups.length - 1];
    return closeSpecificSymbolPopup(latest);
  }

  function getNaturalPageHeight() {
    const bodyH = (document.body && document.body.offsetHeight) || 0;
    const winH = (document.documentElement && document.documentElement.clientHeight) || window.innerHeight || 0;
    return Math.max(bodyH, winH);
  }

  function getFloatingTopClearance() {
    const appHeader = document.querySelector('header.app-header');
    let clearance = appHeader ? Math.max(0, (appHeader.getBoundingClientRect ? appHeader.getBoundingClientRect().bottom : (appHeader.offsetHeight || 0))) : 0;

    const sections = document.querySelectorAll('.diff-file-section:not(.collapsed)');
    for (let i = 0; i < sections.length; i++) {
      const sec = sections[i];
      const hdr = sec.querySelector('.diff-file-header');
      if (!hdr || !sec.getBoundingClientRect || !hdr.getBoundingClientRect) continue;
      const secRect = sec.getBoundingClientRect();
      const hdrRect = hdr.getBoundingClientRect();
      if (hdrRect.height > 0 && secRect.bottom > hdrRect.bottom && hdrRect.top <= clearance + 1.5 && hdrRect.bottom > clearance) {
        clearance = Math.max(clearance, hdrRect.bottom);
      }
    }
    return clearance;
  }
  window.getFloatingTopClearance = getFloatingTopClearance;

  function getSnapTopClearance() {
    const appHeader = document.querySelector('header.app-header');
    const appRect = appHeader && appHeader.getBoundingClientRect ? appHeader.getBoundingClientRect() : null;
    const appH = appHeader
      ? Math.max(0, (appRect && appRect.bottom > 0) ? appRect.bottom : ((appRect && appRect.height > 0) ? appRect.height : (appHeader.offsetHeight || 0)))
      : 0;

    const hangingHdr = document.querySelector('.diff-file-section:not(.collapsed) .diff-file-header.is-hanging');
    if (hangingHdr && hangingHdr.getBoundingClientRect) {
      const sec = hangingHdr.closest ? hangingHdr.closest('.diff-file-section') : null;
      const secRect = sec && sec.getBoundingClientRect ? sec.getBoundingClientRect() : null;
      const hdrRect = hangingHdr.getBoundingClientRect();
      if ((!secRect || secRect.bottom >= hdrRect.bottom) && hdrRect.top >= appH - 1 && hdrRect.bottom > appH) {
        return hdrRect.bottom;
      }
    }

    let clearance = appH;
    const expandedHdr = document.querySelector('.diff-file-section:not(.collapsed) .diff-file-header');
    const sampleFileHdr = expandedHdr || document.querySelector('.diff-file-header');
    if (sampleFileHdr) {
      const hdrRect = sampleFileHdr.getBoundingClientRect ? sampleFileHdr.getBoundingClientRect() : null;
      let hdrH = (hdrRect && hdrRect.height > 0) ? hdrRect.height : (sampleFileHdr.offsetHeight || 35);
      if (!expandedHdr) {
        const collapsedSec = sampleFileHdr.closest ? sampleFileHdr.closest('.diff-file-section.collapsed') : null;
        if (collapsedSec) {
          hdrH += 1;
        }
      }
      clearance = appH + hdrH;
    }
    return clearance;
  }
  window.getSnapTopClearance = getSnapTopClearance;

  function getPopupTopClearance() {
    return Math.max(getFloatingTopClearance(), getSnapTopClearance());
  }
  window.getPopupTopClearance = getPopupTopClearance;

  function detectSnapZone(clientX, clientY) {
    const vw = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1024;
    const vh = (document.documentElement && document.documentElement.clientHeight) || window.innerHeight || 768;
    const topClearance = getSnapTopClearance();

    const EDGE_THRESH = 28;
    const nearLeft = clientX <= EDGE_THRESH;
    const nearRight = clientX >= vw - EDGE_THRESH;
    const nearBottom = clientY >= vh - EDGE_THRESH;
    const nearTop = clientY <= topClearance + EDGE_THRESH;

    if (!nearLeft && !nearRight && !nearBottom && !nearTop) {
      return null;
    }

    const CORNER_SPAN = Math.min(96, Math.max(48, Math.floor(Math.min(vw, Math.max(200, vh - topClearance)) * 0.18)));

    if ((nearLeft && clientY <= topClearance + CORNER_SPAN) || (nearTop && clientX <= CORNER_SPAN)) {
      return 'top-left';
    }
    if ((nearRight && clientY <= topClearance + CORNER_SPAN) || (nearTop && clientX >= vw - CORNER_SPAN)) {
      return 'top-right';
    }
    if ((nearLeft && clientY >= vh - CORNER_SPAN) || (nearBottom && clientX <= CORNER_SPAN)) {
      return 'bottom-left';
    }
    if ((nearRight && clientY >= vh - CORNER_SPAN) || (nearBottom && clientX >= vw - CORNER_SPAN)) {
      return 'bottom-right';
    }

    if (nearLeft) return 'left';
    if (nearRight) return 'right';
    if (nearTop) return 'top';
    if (nearBottom) return 'bottom';

    return null;
  }
  window.detectSnapZone = detectSnapZone;

  function computeSnapRect(zone) {
    if (!zone) return null;
    const vw = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1024;
    const vh = (document.documentElement && document.documentElement.clientHeight) || window.innerHeight || 768;
    const topClearance = getSnapTopClearance();
    const PAD = 6;

    const workLeft = PAD;
    const workRight = Math.max(workLeft + 260, vw - PAD);
    const workTop = topClearance;
    const workBottom = Math.max(workTop + 120, vh - PAD);

    const workW = workRight - workLeft;
    const workH = workBottom - workTop;

    const halfW = Math.floor((workW - PAD) / 2);
    const rightX = workLeft + halfW + PAD;
    const rightW = workRight - rightX;

    const halfH = Math.floor((workH - PAD) / 2);
    const bottomY = workTop + halfH + PAD;
    const bottomH = workBottom - bottomY;

    switch (zone) {
      case 'left':
        return { left: workLeft, top: workTop, width: halfW, height: workH };
      case 'right':
        return { left: rightX, top: workTop, width: rightW, height: workH };
      case 'top':
        return { left: workLeft, top: workTop, width: workW, height: halfH };
      case 'bottom':
        return { left: workLeft, top: bottomY, width: workW, height: bottomH };
      case 'top-left':
        return { left: workLeft, top: workTop, width: halfW, height: halfH };
      case 'top-right':
        return { left: rightX, top: workTop, width: rightW, height: halfH };
      case 'bottom-left':
        return { left: workLeft, top: bottomY, width: halfW, height: bottomH };
      case 'bottom-right':
        return { left: rightX, top: bottomY, width: rightW, height: bottomH };
      default:
        return null;
    }
  }
  window.computeSnapRect = computeSnapRect;

  let snapPreviewEl = null;

  function showSnapPreview(rect, zIndex) {
    if (!rect) {
      hideSnapPreview();
      return;
    }
    if (!snapPreviewEl) {
      snapPreviewEl = document.createElement('div');
      snapPreviewEl.className = 'sym-snap-preview';
      document.body.appendChild(snapPreviewEl);
    }
    snapPreviewEl.style.left = `${rect.left}px`;
    snapPreviewEl.style.top = `${rect.top}px`;
    snapPreviewEl.style.width = `${rect.width}px`;
    snapPreviewEl.style.height = `${rect.height}px`;
    snapPreviewEl.style.zIndex = String(Math.max(10, (zIndex || 200) - 1));
    snapPreviewEl.style.display = 'block';
    if (Math.abs(rect.top - getSnapTopClearance()) <= 1) {
      snapPreviewEl.classList.add('sym-snapped-top');
    } else {
      snapPreviewEl.classList.remove('sym-snapped-top');
    }
  }

  function hideSnapPreview() {
    if (snapPreviewEl) {
      snapPreviewEl.style.display = 'none';
      const p = snapPreviewEl.parentNode || snapPreviewEl.parentElement;
      if (p && p.removeChild) {
        p.removeChild(snapPreviewEl);
      }
      snapPreviewEl = null;
    }
  }
  window.showSnapPreview = showSnapPreview;
  window.hideSnapPreview = hideSnapPreview;

  function applyPopupSnap(inst, zone) {
    if (!inst || !inst.el) return;
    const popup = inst.el;
    const rect = computeSnapRect(zone);
    if (!rect) return;

    if (inst.shaded) {
      setPopupShaded(inst, false, false);
    }

    if (!inst.snapped) {
      inst.preSnapSize = {
        width: parseFloat(popup.style.width) || popup.offsetWidth || 680,
        height: inst.resizedH ? (parseFloat(popup.style.height) || popup.offsetHeight || 360) : null,
        resizedW: !!inst.resizedW,
        resizedH: !!inst.resizedH,
      };
    }

    inst.snapped = true;
    inst.snapZone = zone;
    inst.customSnapRect = false;

    popup.classList.add('sym-snapped');
    popup.classList.add('sym-resized-h');
    const isTopSnapped = ['left', 'right', 'top', 'top-left', 'top-right'].includes(zone);
    if (isTopSnapped) {
      popup.classList.add('sym-snapped-top');
    } else {
      popup.classList.remove('sym-snapped-top');
    }
    popup.style.position = 'fixed';
    popup.style.maxHeight = 'none';
    popup.style.maxWidth = 'none';
    popup.style.left = `${rect.left}px`;
    popup.style.top = `${rect.top}px`;
    popup.style.width = `${rect.width}px`;
    popup.style.height = `${rect.height}px`;
    popup.style.transform = '';
  }
  window.applyPopupSnap = applyPopupSnap;

  function updateSnappedPopupsLayout() {
    const popups = window.openSymbolPopups;
    if (!popups || popups.length === 0) return;
    const vh = (document.documentElement && document.documentElement.clientHeight) || window.innerHeight || 768;
    const topClearance = getSnapTopClearance();

    for (const inst of popups) {
      if (!inst || !inst.el || !inst.snapped) continue;
      const popup = inst.el;

      if (inst.snapZone && !inst.customSnapRect) {
        const rect = computeSnapRect(inst.snapZone);
        if (rect) {
          popup.style.left = `${rect.left}px`;
          popup.style.top = `${rect.top}px`;
          popup.style.width = `${rect.width}px`;
          popup.style.height = `${rect.height}px`;
        }
        const isTopSnapped = ['left', 'right', 'top', 'top-left', 'top-right'].includes(inst.snapZone);
        if (isTopSnapped) {
          popup.classList.add('sym-snapped-top');
        } else {
          popup.classList.remove('sym-snapped-top');
        }
      } else if (inst.customSnapRect) {
        const minTop = topClearance;
        let curTop = parseFloat(popup.style.top) || minTop;
        let curH = popup.offsetHeight || 120;
        if (curTop < minTop) {
          const diff = minTop - curTop;
          curTop = minTop;
          curH = Math.max(120, curH - diff);
          popup.style.top = `${curTop}px`;
          popup.style.height = `${curH}px`;
        } else if (curTop + curH > vh - 6) {
          curH = Math.max(120, (vh - 6) - curTop);
          popup.style.height = `${curH}px`;
        }
        if (Math.abs((parseFloat(popup.style.top) || 0) - topClearance) <= 1) {
          popup.classList.add('sym-snapped-top');
        } else {
          popup.classList.remove('sym-snapped-top');
        }
      }
    }
  }

  function getPopupMinPageTop() {
    return Math.max(12, getPopupTopClearance() + 6);
  }

  let lastNaturalPageHeight = getNaturalPageHeight();

  function handleNaturalPageHeightChange() {
    const curHeight = getNaturalPageHeight();
    if (curHeight < lastNaturalPageHeight) {
      const minPageTop = getPopupMinPageTop();
      const popups = window.openSymbolPopups;
      if (popups) {
        for (const inst of popups) {
          if (!inst || !inst.el || inst.snapped) continue;
          const popTop = parseFloat(inst.el.style.top) || 0;
          const popHeight = inst.el.offsetHeight || 200;
          const wasOverflowing = (popTop + popHeight > lastNaturalPageHeight);
          if (!wasOverflowing) {
            if (popTop + popHeight > curHeight - 12) {
              const clampedTop = Math.max(minPageTop, curHeight - popHeight - 12);
              inst.el.style.top = `${clampedTop}px`;
            }
          } else {
            if (popTop > curHeight - 36) {
              const clampedTop = Math.max(minPageTop, curHeight - 36);
              inst.el.style.top = `${clampedTop}px`;
            }
          }
        }
      }
    }
    lastNaturalPageHeight = curHeight;
  }

  if (typeof ResizeObserver === 'function' && document.body) {
    try {
      new ResizeObserver(handleNaturalPageHeightChange).observe(document.body);
    } catch (_) {}
  }
  window.handleNaturalPageHeightChange = handleNaturalPageHeightChange;

  function relocatePopupsAfterCollapse(collapsedRegions) {
    if (!collapsedRegions || collapsedRegions.length === 0 || openSymbolPopups.length === 0) {
      lastNaturalPageHeight = getNaturalPageHeight();
      return;
    }
    const pageHeight = getNaturalPageHeight();
    const minPageTop = getPopupMinPageTop();
    const scrollY = window.scrollY || 0;

    for (const r of collapsedRegions) {
      if (r.secEl && r.secEl.getBoundingClientRect) {
        r.newBottom = r.secEl.getBoundingClientRect().bottom + scrollY;
      } else {
        r.newBottom = r.secTop;
      }
    }

    const popups = window.openSymbolPopups;
    if (!popups || popups.length === 0) {
      lastNaturalPageHeight = pageHeight;
      return;
    }

    for (const inst of popups) {
      if (!inst || !inst.el || inst.snapped) continue;
      const popTop = parseFloat(inst.el.style.top) || 0;
      const popHeight = inst.el.offsetHeight || 200;
      let nextTop = popTop;

      for (const r of collapsedRegions) {
        if (popTop >= r.secTop && popTop < r.oldBottom) {
          if (popTop > r.newBottom) {
            nextTop = r.newBottom;
          }
        }
      }

      const wasOverflowing = (popTop + popHeight > lastNaturalPageHeight);
      if (!wasOverflowing) {
        if (nextTop + popHeight > pageHeight - 12) {
          nextTop = Math.max(minPageTop, pageHeight - popHeight - 12);
        }
      } else {
        if (nextTop > pageHeight - 36) {
          nextTop = Math.max(minPageTop, pageHeight - 36);
        }
      }

      if (nextTop !== popTop) {
        nextTop = Math.max(minPageTop, nextTop);
        inst.el.style.top = `${nextTop}px`;
      }
    }

    lastNaturalPageHeight = pageHeight;
  }

  function getPopupContentEl(popupOrInst) {
    if (!popupOrInst) return null;
    if (popupOrInst.contentEl) return popupOrInst.contentEl;
    if (popupOrInst.el && popupOrInst.el.contentEl) return popupOrInst.el.contentEl;
    const el = popupOrInst.el || popupOrInst;
    if (el.querySelector) {
      const inner = el.querySelector('.sym-popup-content');
      if (inner) return inner;
    }
    return el;
  }

  function positionSymbolPopup(inst, tok) {
    if (!inst || !inst.el || !tok) return;
    const popup = inst.el;
    const rect = tok.getBoundingClientRect ? tok.getBoundingClientRect() : { left: 0, top: 0, bottom: 0, right: 0, width: 0, height: 0 };
    const pageTokLeft = rect.left + (window.scrollX || 0);
    const pageTokTop = rect.top + (window.scrollY || 0);
    const pageTokBottom = rect.bottom + (window.scrollY || 0);

    const minVTop = (window.scrollY || 0) + Math.max(12, getPopupTopClearance() + 6);

    const clientW = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1024;
    const popupWidth = inst.resizedW ? (popup.offsetWidth || 680) : Math.min(680, clientW - 24);
    const maxPopupLeft = Math.max(12, (window.scrollX || 0) + clientW - popupWidth - 12);
    let left = Math.max(12, Math.min(pageTokLeft, maxPopupLeft));
    let top = Math.max(minVTop, pageTokBottom + 6);

    // Cascade if colliding within 8px of an existing open popup
    let collision = true;
    let iterations = 0;
    while (collision && iterations < 10) {
      collision = false;
      for (const other of openSymbolPopups) {
        if (other === inst || !other.el || (document.body && document.body.contains && !document.body.contains(other.el))) continue;
        let oLeft = parseFloat(other.el.style.left) || 0;
        let oTop = parseFloat(other.el.style.top) || 0;
        if (other.snapped) {
          oLeft += (window.scrollX || 0);
          oTop += (window.scrollY || 0);
        }
        if (Math.abs(left - oLeft) < 8 && Math.abs(top - oTop) < 8) {
          left = Math.min(left + 18, maxPopupLeft);
          top += 18;
          collision = true;
          break;
        }
      }
      iterations++;
    }

    popup.style.left = `${left}px`;
    popup.style.top = `${top}px`;
  }

  function setupDragAndResize(inst) {
    if (!inst || !inst.el) return;
    const popup = inst.el;
    const MIN_HEIGHT = 120;

    popup.addEventListener('pointerdown', (e) => {
      if (e.button !== 0) return;
      if (inst._cleanupActivePointer) {
        inst._cleanupActivePointer();
      }
      bringPopupToFront(inst);
      activePopupInst = inst;
      if (inst.el && inst.el.focus) inst.el.focus({ preventScroll: true });

      const resizeHandle = e.target.closest ? e.target.closest('.sym-resize-handle') : null;
      if (resizeHandle) {
        const dir = (resizeHandle.getAttribute && resizeHandle.getAttribute('data-resize-dir')) || '';
        startResize(e, resizeHandle, dir);
        return;
      }

      if (e.target.closest && e.target.closest('button, .sym-popup-actions a')) {
        return;
      }

      const header = e.target.closest ? e.target.closest('.sym-popup-header') : null;
      if (header) {
        const locLink = e.target.closest ? e.target.closest('a.sym-popup-loc-link') : null;
        startDrag(e, popup, !!locLink);
        return;
      }

      // a) Native scrollbar on any scrollable ancestor
      for (let sc = e.target; sc && sc !== popup; sc = sc.parentElement) {
        if (sc.getBoundingClientRect) {
          const r = sc.getBoundingClientRect();
          if (sc.offsetWidth > sc.clientWidth && sc.clientWidth > 0 && e.clientX >= r.left + (sc.clientLeft || 0) + sc.clientWidth) {
            return;
          }
          if (sc.offsetHeight > sc.clientHeight && sc.clientHeight > 0 && e.clientY >= r.top + (sc.clientTop || 0) + sc.clientHeight) {
            return;
          }
        }
      }

      // b) Interactive rows or inline links/tokens
      if (e.target.closest && e.target.closest('.sym-ref-row, .sym-impl-row, [data-resolve-sym], a[href], span[class*="tok-"], [class*="tok-"], code, mark')) {
        return;
      }

      // c) Rendered non-whitespace code/text on that visual line inside .sym-snippet-code or .sym-popup-sig
      const codeBox = e.target.closest ? e.target.closest('.sym-snippet-code, .sym-popup-sig') : null;
      if (codeBox && isPointOverCodeLineSpan(codeBox, e.clientX, e.clientY)) {
        return;
      }

      startDrag(e, popup, false);
    });

    function isPointOverCodeLineSpan(codeBox, clientX, clientY) {
      if (!codeBox) return false;
      const text = (codeBox.textContent || '').trim();
      if (text === '') return false;

      let minLeft = Infinity;
      let maxRight = -Infinity;

      const children = (codeBox.childNodes && codeBox.childNodes.length > 0) ? codeBox.childNodes : (codeBox.children || []);
      for (let i = 0; i < children.length; i++) {
        const node = children[i];
        if (node.nodeType === 1 || node.tagName) {
          if ((node.textContent || '').trim() !== '' && node.getBoundingClientRect) {
            const r = node.getBoundingClientRect();
            if (r && r.width > 0 && r.height > 0 && clientY >= r.top - 2 && clientY <= r.bottom + 2) {
              minLeft = Math.min(minLeft, r.left);
              maxRight = Math.max(maxRight, r.right);
            }
          }
        } else if (node.nodeType === 3 /* TEXT_NODE */) {
          const val = node.nodeValue || '';
          const leadingM = val.match(/^\s*/);
          const trailingM = val.match(/\s*$/);
          const s = leadingM ? leadingM[0].length : 0;
          const eOff = val.length - (trailingM ? trailingM[0].length : 0);
          if (s < eOff && typeof document.createRange === 'function') {
            try {
              const range = document.createRange();
              range.setStart(node, s);
              range.setEnd(node, eOff);
              const rects = range.getClientRects ? range.getClientRects() : [];
              for (let j = 0; j < rects.length; j++) {
                const r = rects[j];
                if (r && r.width > 0 && r.height > 0 && clientY >= r.top - 2 && clientY <= r.bottom + 2) {
                  minLeft = Math.min(minLeft, r.left);
                  maxRight = Math.max(maxRight, r.right);
                }
              }
            } catch (_) {}
          }
        }
      }

      if (minLeft <= maxRight && clientX >= minLeft - 3 && clientX <= maxRight + 3) {
        return true;
      }
      return false;
    }

    popup.addEventListener('focusin', () => {
      bringPopupToFront(inst);
    });

    function getHeaderClearanceOffset() {
      return Math.max(12, getPopupTopClearance() + 6);
    }

    function startResize(e, handleEl, dir) {
      if (e.button !== 0) return;
      if (e.preventDefault) e.preventDefault();
      if (e.stopPropagation) e.stopPropagation();

      const isSnapped = !!inst.snapped;
      const effectiveMinW = getPopupMinWidth(popup);

      const pointerId = e.pointerId;
      let hasCapture = false;
      if (pointerId !== undefined && handleEl && handleEl.setPointerCapture) {
        try {
          handleEl.setPointerCapture(pointerId);
          hasCapture = true;
        } catch (_) {}
      }

      const startX = e.clientX;
      const startY = e.clientY;
      const startScrollX = window.scrollX || 0;
      const startScrollY = window.scrollY || 0;
      const startRect = popup.getBoundingClientRect ? popup.getBoundingClientRect() : { left: 0, top: 0, width: 260, height: 120 };
      const startLeft = parseFloat(popup.style.left) || (isSnapped ? startRect.left : (startRect.left + startScrollX));
      const startTop = parseFloat(popup.style.top) || (isSnapped ? startRect.top : (startRect.top + startScrollY));
      const startWidth = popup.offsetWidth || startRect.width;
      const startHeight = popup.offsetHeight || startRect.height;

      if (isSnapped) {
        inst.customSnapRect = true;
      }

      if (popup.classList && popup.classList.add) {
        popup.classList.add('sym-resizing');
      }

      let nextLeft = startLeft;
      let nextTop = startTop;
      let nextWidth = startWidth;
      let nextHeight = startHeight;
      let updateW = false;
      let updateLeft = false;
      let updateH = false;
      let updateTop = false;
      let appliedResizedH = popup.classList && popup.classList.contains ? popup.classList.contains('sym-resized-h') : false;
      let dirty = false;
      let rafId = 0;

      function applyResizeFrame() {
        rafId = 0;
        if (!dirty) return;
        dirty = false;
        if (updateW) {
          inst.resizedW = true;
          popup.style.width = `${nextWidth}px`;
        }
        if (updateLeft) {
          popup.style.left = `${nextLeft}px`;
        }
        if (updateH) {
          inst.resizedH = true;
          if (!appliedResizedH) {
            appliedResizedH = true;
            popup.style.maxHeight = 'none';
            if (popup.classList && popup.classList.add) popup.classList.add('sym-resized-h');
          }
          popup.style.height = `${nextHeight}px`;
        }
        if (updateTop) {
          popup.style.top = `${nextTop}px`;
          if (isSnapped) {
            if (Math.abs(nextTop - getSnapTopClearance()) <= 1) {
              popup.classList.add('sym-snapped-top');
            } else {
              popup.classList.remove('sym-snapped-top');
            }
          }
        }
      }

      function cleanupPointer() {
        if (rafId && typeof cancelAnimationFrame === 'function') {
          cancelAnimationFrame(rafId);
          rafId = 0;
        }
        if (dirty) {
          applyResizeFrame();
        }
        window.removeEventListener('pointermove', onPointerMove, true);
        window.removeEventListener('pointerup', onPointerUp, true);
        window.removeEventListener('pointercancel', onPointerUp, true);
        if (hasCapture && handleEl && handleEl.releasePointerCapture) {
          try { handleEl.releasePointerCapture(pointerId); } catch (_) {}
          hasCapture = false;
        }
        if (popup.classList && popup.classList.remove) {
          popup.classList.remove('sym-resizing');
        }
        inst._cleanupActivePointer = null;
      }
      inst._cleanupActivePointer = cleanupPointer;

      function onPointerMove(moveEvt) {
        let dx, dy, minHeaderTop;
        if (isSnapped) {
          dx = moveEvt.clientX - startX;
          dy = moveEvt.clientY - startY;
          minHeaderTop = getSnapTopClearance();
        } else {
          const curScrollX = window.scrollX || 0;
          const curScrollY = window.scrollY || 0;
          dx = (moveEvt.clientX + curScrollX) - (startX + startScrollX);
          dy = (moveEvt.clientY + curScrollY) - (startY + startScrollY);
          minHeaderTop = curScrollY + getHeaderClearanceOffset();
        }

        const vw = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1024;
        const vh = (document.documentElement && document.documentElement.clientHeight) || window.innerHeight || 768;

        if (dir.includes('e')) {
          if (isSnapped) {
            nextWidth = Math.min(Math.max(effectiveMinW, (vw - 6) - startLeft), Math.max(effectiveMinW, startWidth + dx));
          } else {
            nextWidth = Math.max(effectiveMinW, startWidth + dx);
          }
          updateW = true;
        } else if (dir.includes('w')) {
          let newLeft = startLeft + dx;
          let newWidth = startWidth - dx;
          const minLeft = isSnapped ? 6 : 0;
          if (newLeft < minLeft) {
            newLeft = minLeft;
            newWidth = Math.max(effectiveMinW, startLeft + startWidth - minLeft);
          } else if (newWidth < effectiveMinW) {
            newWidth = effectiveMinW;
            newLeft = startLeft + (startWidth - effectiveMinW);
          }
          nextWidth = newWidth;
          nextLeft = newLeft;
          updateW = true;
          updateLeft = true;
        }

        if (dir.includes('s')) {
          if (isSnapped) {
            nextHeight = Math.min(Math.max(MIN_HEIGHT, (vh - 6) - startTop), Math.max(MIN_HEIGHT, startHeight + dy));
          } else {
            nextHeight = Math.max(MIN_HEIGHT, startHeight + dy);
          }
          updateH = true;
        } else if (dir.includes('n')) {
          let newTop = startTop + dy;
          let newHeight = startHeight - dy;
          if (newTop < minHeaderTop) {
            newTop = minHeaderTop;
            newHeight = Math.max(MIN_HEIGHT, (startTop + startHeight) - minHeaderTop);
          } else if (newHeight < MIN_HEIGHT) {
            newHeight = MIN_HEIGHT;
            newTop = startTop + (startHeight - MIN_HEIGHT);
          }
          nextHeight = newHeight;
          nextTop = newTop;
          updateH = true;
          updateTop = true;
        }

        dirty = true;
        if (typeof requestAnimationFrame === 'function') {
          if (!rafId) rafId = requestAnimationFrame(applyResizeFrame);
        } else {
          applyResizeFrame();
        }
      }

      function onPointerUp() {
        cleanupPointer();
      }

      window.addEventListener('pointermove', onPointerMove, true);
      window.addEventListener('pointerup', onPointerUp, true);
      window.addEventListener('pointercancel', onPointerUp, true);
    }

    function startDrag(e, headerEl, isLocLink) {
      if (e.button !== 0) return;
      if (!isLocLink && e.preventDefault) {
        e.preventDefault();
      }
      const pointerId = e.pointerId;
      let hasCapture = false;
      const startX = e.clientX;
      const startY = e.clientY;
      const isSnapped = !!inst.snapped;
      const startScrollX = window.scrollX || 0;
      const startScrollY = window.scrollY || 0;
      const startRect = popup.getBoundingClientRect ? popup.getBoundingClientRect() : { left: 0, top: 0, width: 260, height: 120 };
      let startLeft = parseFloat(popup.style.left) || (isSnapped ? startRect.left : (startRect.left + startScrollX));
      let startTop = parseFloat(popup.style.top) || (isSnapped ? startRect.top : (startRect.top + startScrollY));
      const viewportW = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1024;
      let hasDragged = false;
      let activeSnapZone = null;
      const threshold = isLocLink ? 4 : 2;
      let pendingLeft = startLeft;
      let pendingTop = startTop;
      let rafId = 0;

      function applyDragFrame() {
        rafId = 0;
        if (!hasDragged) return;
        popup.style.transform = `translate3d(${pendingLeft - startLeft}px, ${pendingTop - startTop}px, 0)`;
      }

      function cleanupPointer(isSnapping) {
        hideSnapPreview();
        if (rafId && typeof cancelAnimationFrame === 'function') {
          cancelAnimationFrame(rafId);
          rafId = 0;
        }
        if (hasDragged) {
          popup.style.transform = '';
          if (!isSnapping) {
            popup.style.left = `${pendingLeft}px`;
            popup.style.top = `${pendingTop}px`;
          }
        }
        window.removeEventListener('pointermove', onPointerMove, true);
        window.removeEventListener('pointerup', onPointerUp, true);
        window.removeEventListener('pointercancel', onPointerCancel, true);
        if (hasCapture && popup && popup.releasePointerCapture) {
          try { popup.releasePointerCapture(pointerId); } catch (_) {}
          hasCapture = false;
        }
        if (popup.classList && popup.classList.remove) {
          popup.classList.remove('sym-dragging');
        }
        inst._cleanupActivePointer = null;
      }
      inst._cleanupActivePointer = () => cleanupPointer(false);

      function onPointerMove(moveEvt) {
        const curScrollX = window.scrollX || 0;
        const curScrollY = window.scrollY || 0;
        const dx = (moveEvt.clientX + curScrollX) - (startX + startScrollX);
        const dy = (moveEvt.clientY + curScrollY) - (startY + startScrollY);

        if (!hasDragged) {
          if (Math.hypot(dx, dy) < threshold) return;
          hasDragged = true;
          inst.moved = true;

          if (isSnapped) {
            const snapRect = {
              left: parseFloat(popup.style.left) || startRect.left,
              top: parseFloat(popup.style.top) || startRect.top,
              width: popup.offsetWidth || startRect.width,
              height: popup.offsetHeight || startRect.height,
            };
            const ratioX = Math.max(0.1, Math.min(0.9, (startX - snapRect.left) / Math.max(1, snapRect.width)));
            const offsetY = Math.max(8, Math.min(24, startY - snapRect.top));

            const pre = inst.preSnapSize || { width: 680, height: null, resizedW: false, resizedH: false };
            inst.snapped = false;
            inst.snapZone = null;
            inst.customSnapRect = false;
            popup.classList.remove('sym-snapped');
            popup.classList.remove('sym-snapped-top');
            popup.style.position = '';
            popup.style.maxHeight = '';
            popup.style.maxWidth = '';

            inst.resizedW = !!pre.resizedW;
            if (pre.resizedW && pre.width) {
              popup.style.width = `${pre.width}px`;
            } else {
              popup.style.width = '';
            }
            const restoredW = popup.offsetWidth || pre.width || 680;

            if (pre.resizedH && pre.height) {
              popup.style.height = `${pre.height}px`;
              popup.style.maxHeight = 'none';
              inst.resizedH = true;
              if (popup.classList && popup.classList.add) popup.classList.add('sym-resized-h');
            } else {
              popup.style.height = '';
              popup.style.maxHeight = '';
              inst.resizedH = false;
              if (popup.classList && popup.classList.remove) popup.classList.remove('sym-resized-h');
            }

            const topClearance = getPopupTopClearance();
            startLeft = (startX + curScrollX) - restoredW * ratioX;
            startTop = Math.max(curScrollY + topClearance + 6, (startY + curScrollY) - offsetY);
          }

          popup.style.left = `${startLeft}px`;
          popup.style.top = `${startTop}px`;
          if (popup.classList && popup.classList.add) {
            popup.classList.add('sym-dragging');
          }
          if (pointerId !== undefined && popup && popup.setPointerCapture) {
            try {
              popup.setPointerCapture(pointerId);
              hasCapture = true;
            } catch (_) {}
          }
          if (isLocLink && window.getSelection) {
            const sel = window.getSelection();
            if (sel && !sel.isCollapsed && sel.removeAllRanges) sel.removeAllRanges();
          }
        }

        const minHeaderTop = curScrollY + getHeaderClearanceOffset();
        const maxLeft = curScrollX + viewportW - 80;
        pendingLeft = Math.max(0, Math.min(startLeft + dx, maxLeft));
        pendingTop = Math.max(minHeaderTop, startTop + dy);

        activeSnapZone = detectSnapZone(moveEvt.clientX, moveEvt.clientY);
        if (activeSnapZone) {
          const snapRect = computeSnapRect(activeSnapZone);
          const zIdx = (parseInt(popup.style.zIndex, 10) || 200) - 1;
          showSnapPreview(snapRect, zIdx);
        } else {
          hideSnapPreview();
        }

        if (typeof requestAnimationFrame === 'function') {
          if (!rafId) rafId = requestAnimationFrame(applyDragFrame);
        } else {
          applyDragFrame();
        }
      }

      function onPointerUp(upEvt) {
        const snapTarget = activeSnapZone;
        activeSnapZone = null;
        hideSnapPreview();
        cleanupPointer(!!snapTarget);
        if (hasDragged && snapTarget) {
          applyPopupSnap(inst, snapTarget);
        }
        if (hasDragged && isLocLink) {
          inst._suppressNextLinkClick = true;
          setTimeout(() => { inst._suppressNextLinkClick = false; }, 100);
        }
      }

      function onPointerCancel() {
        activeSnapZone = null;
        hideSnapPreview();
        cleanupPointer(false);
      }

      window.addEventListener('pointermove', onPointerMove, true);
      window.addEventListener('pointerup', onPointerUp, true);
      window.addEventListener('pointercancel', onPointerCancel, true);
    }
  }
  window.setupDragAndResize = setupDragAndResize;

  window.addEventListener('scroll', scheduleStickyScopeUpdate, { passive: true, capture: true });
  window.addEventListener('resize', scheduleStickyScopeUpdate, { passive: true });
  window.addEventListener('resize', syncAppHeaderHeight, { passive: true });
  window.addEventListener('resize', updateSnappedPopupsLayout, { passive: true });

  var appHeaderResizeObserved = false;
  function observeAppHeaderResize() {
    const appHeader = document.querySelector('header.app-header');
    if (!appHeader) return;
    syncAppHeaderHeight();
    if (!appHeaderResizeObserved && typeof ResizeObserver === 'function') {
      appHeaderResizeObserved = true;
      try {
        new ResizeObserver(() => {
          syncAppHeaderHeight();
          if (typeof updateSnappedPopupsLayout === 'function') {
            updateSnappedPopupsLayout();
          }
        }).observe(appHeader);
      } catch (_) {}
    }
  }
  observeAppHeaderResize();

  async function handleJumpToDefinition(repo, change, defPath, defLine, lookupSide, e) {
    if (e && (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0)) {
      return;
    }
    if (e) e.preventDefault();

    const cleanDefPath = (defPath || '').replace(/^\/+/, '');
    let fileSec = null;
    let container = null;
    fileSec = document.querySelector(`.diff-file-section[data-file-path="${escapeAttrVal(cleanDefPath)}"]`);
    container = document.querySelector(`.diff-container[data-file-path="${escapeAttrVal(cleanDefPath)}"]`);
    if (container && container.hasAttribute && container.hasAttribute('data-ext-pkg')) {
      container = null;
    }

    if (fileSec || container) {
      if (fileSec) {
        const body = fileSec.querySelector('.diff-file-body');
        if (body && (body.style.display === 'none' || fileSec.classList.contains('collapsed'))) {
          const btn = fileSec.querySelector('.diff-file-header button');
          body.style.display = 'block';
          fileSec.classList.remove('collapsed');
          if (btn) btn.textContent = '▾';
        }
        if (body && window.ensureFileBodyLoaded && (body.hasAttribute('data-lazy-url') || body._loadPromise)) {
          await window.ensureFileBodyLoaded(body);
        }
      }

      const parentScope = fileSec || container;
      const skipRows = parentScope.querySelectorAll('tr.row-skip');
      for (const skip of skipRows) {
        const lStart = parseInt(skip.getAttribute('data-left-start') || '0', 10);
        const lEnd = parseInt(skip.getAttribute('data-left-end') || '0', 10);
        const rStart = parseInt(skip.getAttribute('data-right-start') || '0', 10);
        const rEnd = parseInt(skip.getAttribute('data-right-end') || '0', 10);
        const inLeft = (lookupSide === 'L' && defLine >= lStart && defLine <= lEnd);
        const inRight = (lookupSide !== 'L' && defLine >= rStart && defLine <= rEnd);
        if (inLeft || inRight) {
          const expandBtn = skip.querySelector('.skip-all') || skip.querySelector('button');
          if (expandBtn && window.expandContextAll) {
            await window.expandContextAll(expandBtn);
          }
          break;
        }
      }

      const lineAttr = lookupSide === 'L' ? `data-line-l="${defLine}"` : `data-line-r="${defLine}"`;
      let targetRow = parentScope.querySelector(`tr.diff-row[${lineAttr}]`);
      if (!targetRow) {
        targetRow = parentScope.querySelector(`tr[data-line-r="${defLine}"]`) || parentScope.querySelector(`tr[data-line-l="${defLine}"]`);
      }

      if (targetRow) {
        targetRow.scrollIntoView({ block: 'center' });
        flashDiffRows(targetRow, lookupSide);
        return;
      }
    }

    const anyContainer = document.querySelector('.diff-container');
    const allFilesEl = document.querySelector('.diff-all-files');
    const finalRepo = repo || (container && container.getAttribute('data-repo')) || (anyContainer && anyContainer.getAttribute('data-repo')) || (allFilesEl && allFilesEl.getAttribute('data-repo')) || '';
    const finalChange = change || (container && container.getAttribute('data-change')) || (anyContainer && anyContainer.getAttribute('data-change')) || (allFilesEl && allFilesEl.getAttribute('data-change')) || '';
    const sp = new URLSearchParams(window.location.search);
    const base = sp.get('base') || '';
    const target = sp.get('target') || '';

    window.location.href = buildInRepoJumpHref({
      repo: finalRepo,
      change: finalChange,
      base: base,
      target: target,
      side: lookupSide || 'R',
      mode: getCurrentPageDiffMode(),
    }, cleanDefPath, defLine);
  }
  window.handleJumpToDefinition = handleJumpToDefinition;

  function getCurrentPageDiffMode() {
    if (window.location && window.location.search) {
      const sp = new URLSearchParams(window.location.search);
      const m = sp.get('mode') || sp.get('view');
      if (m === 'split' || m === 'unified') return m;
    }
    if (document.querySelector) {
      if (document.querySelector('.diff-table-split')) return 'split';
      if (document.querySelector('.diff-table:not(.diff-table-single)')) return 'unified';

      const up = document.querySelector('#upLink');
      if (up && up.href) {
        try {
          const u = new URL(up.href, window.location.href);
          const m = u.searchParams.get('mode') || u.searchParams.get('view');
          if (m === 'split' || m === 'unified') return m;
        } catch (_) {}
      }
      const hdrLink = document.querySelector('.app-header a[href*="mode="]');
      if (hdrLink && hdrLink.href) {
        try {
          const u = new URL(hdrLink.href, window.location.href);
          const m = u.searchParams.get('mode') || u.searchParams.get('view');
          if (m === 'split' || m === 'unified') return m;
        } catch (_) {}
      }
      const lazyBody = document.querySelector('.diff-file-body[data-lazy-url*="mode="]');
      if (lazyBody) {
        const lu = lazyBody.getAttribute('data-lazy-url') || '';
        const m = lu.match(/[?&](?:mode|view)=(split|unified)/);
        if (m) return m[1];
      }
    }
    return '';
  }
  window.getCurrentPageDiffMode = getCurrentPageDiffMode;

  function buildInRepoJumpHref(ctx, defPath, defLine) {
    const cleanDef = (defPath || '').replace(/^\/+/, '');
    const mode = (ctx && ctx.mode) || getCurrentPageDiffMode();
    const modeParam = mode ? `&mode=${encodeURIComponent(mode)}` : '';
    return `/r/${encodeURIComponent(ctx.repo)}/c/${encodeURIComponent(ctx.change)}/f/${encodeURIComponent(cleanDef).replace(/%2F/g, '/')}?base=${encodeURIComponent(ctx.base)}&target=${encodeURIComponent(ctx.target)}${modeParam}&ctx=all&side=${encodeURIComponent(ctx.side || 'R')}#L${defLine}`;
  }

  function buildExtJumpHref(ctx, pkg, file, defLine, originPath) {
    const origin = originPath || ctx.originPath || '';
    const mode = (ctx && ctx.mode) || getCurrentPageDiffMode();
    const modeParam = mode ? `&mode=${encodeURIComponent(mode)}` : '';
    return `/r/${encodeURIComponent(ctx.repo)}/c/${encodeURIComponent(ctx.change)}/ext?pkg=${encodeURIComponent(pkg)}&file=${encodeURIComponent(file)}${origin ? '&origin=' + encodeURIComponent(origin) : ''}&base=${encodeURIComponent(ctx.base)}&target=${encodeURIComponent(ctx.target)}${modeParam}&side=${encodeURIComponent(ctx.side || 'R')}#L${defLine}`;
  }

  async function handleJumpToExternalDefinition(repo, change, pkg, file, defLine, lookupSide, e, originPath) {
    if (e && (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0)) {
      return;
    }
    if (!pkg || !file || defLine <= 0) return;
    if (e) e.preventDefault();

    let container = document.querySelector(`.diff-container[data-ext-pkg="${escapeAttrVal(pkg)}"][data-ext-file="${escapeAttrVal(file)}"]`);

    if (container) {
      const targetRow = container.querySelector(`tr.diff-row[data-line-r="${defLine}"]`);
      if (targetRow) {
        targetRow.scrollIntoView({ block: 'center' });
        flashDiffRows(targetRow, lookupSide);
        return;
      }
    }

    const anyContainer = document.querySelector('.diff-container');
    const allFilesEl = document.querySelector('.diff-all-files');
    const finalRepo = repo || (anyContainer && anyContainer.getAttribute('data-repo')) || (allFilesEl && allFilesEl.getAttribute('data-repo')) || '';
    const finalChange = change || (anyContainer && anyContainer.getAttribute('data-change')) || (allFilesEl && allFilesEl.getAttribute('data-change')) || '';
    const sp = new URLSearchParams(window.location.search);
    const base = sp.get('base') || '';
    const target = sp.get('target') || '';

    window.location.href = buildExtJumpHref({
      repo: finalRepo,
      change: finalChange,
      base: base,
      target: target,
      side: lookupSide || 'R',
      mode: getCurrentPageDiffMode(),
    }, pkg, file, defLine, originPath);
  }

  function getJumpNavContext() {
    const inPageRepoPaths = new Set();
    if (document.querySelectorAll) {
      const secs = document.querySelectorAll('.diff-file-section');
      for (const sec of secs) {
        const p = (sec.getAttribute && sec.getAttribute('data-file-path')) || '';
        if (!p) continue;
        const body = sec.querySelector ? sec.querySelector('.diff-file-body') : null;
        const isLazy = !!(body && ((body.hasAttribute && body.hasAttribute('data-lazy-url')) || body._loadPromise));
        const hasTable = !!(sec.querySelector && sec.querySelector('table.diff-table'));
        if (isLazy || hasTable) {
          inPageRepoPaths.add(p.replace(/^\/+/, ''));
        }
      }
      const containers = document.querySelectorAll('.diff-container');
      for (const c of containers) {
        if (c.hasAttribute && c.hasAttribute('data-ext-pkg')) continue;
        const p = (c.getAttribute && c.getAttribute('data-file-path')) || '';
        if (!p) continue;
        const hasRows = !!(c.querySelector && (c.querySelector('table.diff-table') || c.querySelector('tr[data-line-r], tr[data-line-l]')));
        if (hasRows) {
          inPageRepoPaths.add(p.replace(/^\/+/, ''));
        }
      }
    }
    let activeExtPkg = '';
    let activeExtFile = '';
    if (document.querySelectorAll) {
      const containers = document.querySelectorAll('.diff-container');
      for (const c of containers) {
        const ep = (c.getAttribute && c.getAttribute('data-ext-pkg')) || '';
        const ef = (c.getAttribute && c.getAttribute('data-ext-file')) || '';
        if (ep && ef) {
          activeExtPkg = ep;
          activeExtFile = ef;
          break;
        }
      }
    }
    return {
      willRepoNav(defPath, defLine) {
        const clean = (defPath || '').replace(/^\/+/, '');
        if (!clean || !(defLine > 0)) return false;
        return !inPageRepoPaths.has(clean);
      },
      willExtNav(pkg, file, defLine) {
        if (!pkg || !file || !(defLine > 0)) return false;
        return pkg !== activeExtPkg || file !== activeExtFile;
      }
    };
  }

  function renderImplsView(popupOrInst, implsData, stateCtx, restoredScrollTop, restoredScrollLeft) {
    const contentEl = getPopupContentEl(popupOrInst);
    if (!contentEl || !implsData) return;

    const implList = implsData.implementations || implsData.impls || [];
    const backBtnHtml = `<button type="button" class="sym-btn-back" title="Back" aria-label="Back">←</button>`;
    const totalCount = implList.length;
    const badgeText = `${totalCount}${implsData.truncated ? '+' : ''}`;

    const isInterfaces = implsData.direction === 'implements' || implsData.direction === 'interfaces';
    const headerTitle = isInterfaces
      ? `Interfaces Implemented by <code>${escapeHtml(implsData.symbol_name || '')}</code>`
      : `<code>${escapeHtml(implsData.symbol_name || '')}</code> Implementations`;
    const emptyText = isInterfaces ? 'No interfaces found' : 'No implementations found';

    let bodyHtml = `
      <div class="sym-popup-header">
        ${backBtnHtml}
        <div class="sym-popup-loc">
          ${headerTitle}
          <span class="sym-count-badge">${badgeText}</span>
        </div>
        <div class="sym-popup-actions">
          ${renderPopupWindowControlsHtml(popupOrInst)}
        </div>
      </div>
    `;

    if (implsData.signature_html) {
      bodyHtml += `<div class="sym-popup-sig">${implsData.signature_html}</div>`;
    } else if (implsData.signature) {
      bodyHtml += `<div class="sym-popup-sig">${escapeHtml(implsData.signature)}</div>`;
    }

    if (implList.length === 0) {
      bodyHtml += `<div class="sym-list-empty">${emptyText}</div>`;
      contentEl.innerHTML = bodyHtml;
      syncPopupShadeUI(popupOrInst);
      ensurePopupMinWidth(popupOrInst);
      return;
    }

    const groups = [
      { key: 'change', title: 'In changed files' },
      { key: 'package', title: 'In package' },
      { key: 'repo', title: 'In repository' },
      { key: 'external', title: 'In standard library / dependencies' },
    ];

    let listHtml = '';
    for (const g of groups) {
      const gImpls = implList.filter(i => i.group === g.key);
      if (gImpls.length === 0) continue;
      listHtml += `<div class="sym-group-header">${escapeHtml(g.title)} (${gImpls.length})</div>`;
      for (const impl of gImpls) {
        const hasCoords = (impl.in_repo && impl.path && impl.line > 0) || (!impl.in_repo && impl.pkg && impl.pkg_file && impl.line > 0);
        const locLabel = impl.line > 0 ? `${escapeHtml(impl.display)}:${impl.line}` : escapeHtml(impl.display || '');
        const codeHtml = impl.line_html ? `<span class="sym-ref-code">${impl.line_html}</span>` : '';
        if (hasCoords) {
          let targetAttr = '';
          if (impl.in_repo) {
            targetAttr = `data-path="${escapeHtml(impl.path)}"`;
          } else {
            targetAttr = `data-ext="true" data-pkg="${escapeHtml(impl.pkg)}" data-file="${escapeHtml(impl.pkg_file)}"`;
          }
          let nameHtml = `<span class="sym-ref-name">${escapeHtml(impl.name)}</span>`;
          let locHtml = locLabel ? `<span class="sym-ref-loc">${locLabel}</span>` : '';
          listHtml += `
            <div class="sym-ref-row" role="button" tabindex="0" data-resolve-sym="true" data-line="${impl.line}" data-col="${impl.utf16_col || 0}" ${targetAttr}>
              ${nameHtml}
              ${locHtml}
              ${codeHtml}
            </div>
          `;
        } else if (impl.external_url) {
          const extLocHtml = locLabel
            ? `<span class="sym-ref-loc"><span class="sym-loc-text">${locLabel}</span> <span class="sym-nav-arrow" aria-hidden="true">↗</span></span>`
            : '';
          const extNameHtml = locLabel
            ? `<span class="sym-ref-name">${escapeHtml(impl.name)}</span>`
            : `<span class="sym-ref-name">${escapeHtml(impl.name)} <span class="sym-nav-arrow" aria-hidden="true">↗</span></span>`;
          listHtml += `
            <a href="${escapeHtml(impl.external_url)}" target="_blank" rel="noopener noreferrer" class="sym-ref-row sym-will-nav">
              ${extNameHtml}
              ${extLocHtml}
              ${codeHtml}
            </a>
          `;
        } else {
          const staticLocHtml = locLabel ? `<span class="sym-ref-loc">${locLabel}</span>` : '';
          listHtml += `
            <div class="sym-ref-row sym-ref-row-static">
              <span class="sym-ref-name">${escapeHtml(impl.name)}</span>
              ${staticLocHtml}
              ${codeHtml}
            </div>
          `;
        }
      }
    }

    bodyHtml += `<div class="sym-list-body"><div class="sym-list-inner">${listHtml}</div></div>`;
    if (implsData.truncated) {
      bodyHtml += `<div class="sym-snippet-trunc">${isInterfaces ? 'Interface' : 'Implementation'} search truncated at 200 matches</div>`;
    }

    contentEl.innerHTML = bodyHtml;
    syncPopupShadeUI(popupOrInst);
    ensurePopupMinWidth(popupOrInst);
    if (typeof restoredScrollTop === 'number') {
      const listBody = contentEl.querySelector('.sym-list-body');
      if (listBody) {
        listBody.scrollTop = restoredScrollTop;
      }
    }
    if (typeof restoredScrollLeft === 'number') {
      const listBody = contentEl.querySelector('.sym-list-body');
      if (listBody) {
        listBody.scrollLeft = restoredScrollLeft;
      }
    }
    const targetInst = popupOrInst && popupOrInst.el ? popupOrInst : (openSymbolPopups.find(p => p && p.el === popupOrInst) || null);
    if (targetInst && activePopupInst === targetInst && targetInst.el && targetInst.el.focus) {
      targetInst.el.focus({ preventScroll: true });
    }
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  }

  const CALLER_CHEVRON_RIGHT_SVG = '<svg width="13" height="13" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="4 2.2 8.2 6 4 9.8"></polyline></svg>';
  const CALLER_CHEVRON_DOWN_SVG = '<svg width="13" height="13" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="2.2 4.2 6 8.4 9.8 4.2"></polyline></svg>';
  const CALLER_UP_ARROW_SVG = '<svg class="sym-caller-up-arrow" width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12.5 10.5H5a1.5 1.5 0 0 1-1.5-1.5V2.5"></path><polyline points="1.2 5 3.5 2.2 5.8 5"></polyline></svg>';

  function canExpandRef(ref) {
    if (!ref || !ref.container || ref.container_line <= 0) return false;
    return ref.in_repo ? !!ref.path : !!(ref.pkg && ref.pkg_file);
  }

  function getRefDeclKey(ref) {
    if (!ref) return '';
    if (ref.in_repo) {
      return `repo:${ref.path || ''}:${ref.container_line}:${ref.container_utf16_col || 0}`;
    }
    return `ext:${ref.pkg || ''}:${ref.pkg_file || ''}:${ref.container_line}:${ref.container_utf16_col || 0}`;
  }

  function isNodeVisible(nodeId, callerState) {
    let curr = callerState && callerState.nodes ? callerState.nodes[nodeId] : null;
    if (!curr) return false;
    while (curr.parentNodeId) {
      const parent = callerState.nodes[curr.parentNodeId];
      if (!parent || !parent.expanded) return false;
      curr = parent;
    }
    return true;
  }

  function getVisibleCollapsedExpandableNodes(groupKey, callerState) {
    const result = [];
    if (!callerState || !callerState.nodes) return result;
    for (const id in callerState.nodes) {
      const node = callerState.nodes[id];
      if (node.groupKey === groupKey && !node.expanded && canExpandRef(node.ref) && isNodeVisible(id, callerState)) {
        result.push(node);
      }
    }
    return result;
  }

  function hasExpandedChild(nodeId, callerState) {
    if (!callerState || !callerState.nodes) return false;
    const prefix = nodeId + '.';
    for (const id in callerState.nodes) {
      if (id.startsWith(prefix) && callerState.nodes[id].parentNodeId === nodeId && callerState.nodes[id].expanded) {
        return true;
      }
    }
    return false;
  }

  function syncGroupButtons(contentEl, callerState, groupKey) {
    if (!contentEl || !callerState) return;
    const header = contentEl.querySelector(`.sym-group-header[data-ref-group="${groupKey}"]`);
    if (!header) return;
    const expandBtn = header.querySelector('[data-caller-group-action="expand"]');
    const collapseBtn = header.querySelector('[data-caller-group-action="collapse"]');
    if (!expandBtn || !collapseBtn) return;

    const isLoading = !!(callerState.groupLoading && callerState.groupLoading[groupKey]);
    const visibleCollapsed = getVisibleCollapsedExpandableNodes(groupKey, callerState);
    const hasVisibleCollapsed = visibleCollapsed.length > 0;

    let hasAnyExpanded = false;
    for (const id in callerState.nodes) {
      const n = callerState.nodes[id];
      if (n.groupKey === groupKey && n.expanded) {
        hasAnyExpanded = true;
        break;
      }
    }

    expandBtn.disabled = isLoading || !hasVisibleCollapsed;
    collapseBtn.disabled = isLoading || !hasAnyExpanded;
  }

  function hasProp(obj, key) {
    return !!(obj && Object.prototype.hasOwnProperty.call(obj, key));
  }

  async function fetchCallerRefs(ref, stateCtx, callerState) {
    if (!ref || !stateCtx) return [];
    const declKey = getRefDeclKey(ref);
    if (!declKey) return [];

    if (callerState && hasProp(callerState.cache, declKey)) {
      return callerState.cache[declKey];
    }
    if (callerState && hasProp(callerState.inflight, declKey)) {
      return await callerState.inflight[declKey];
    }

    const params = new URLSearchParams({
      repo: stateCtx.repo,
      change: stateCtx.change,
      side: stateCtx.side,
      line: ref.container_line,
      col: ref.container_utf16_col || 0,
      left_rev: stateCtx.leftRev,
      right_rev: stateCtx.rightRev,
      base: stateCtx.base,
      target: stateCtx.target,
    });

    if (ref.in_repo) {
      if (ref.path) params.set('path', ref.path);
    } else {
      params.set('pkg', ref.pkg || '');
      params.set('pkg_file', ref.pkg_file || '');
      if (stateCtx.originPath) params.set('path', stateCtx.originPath);
    }

    const fetchPromise = (async () => {
      const resp = await fetch('/api/symbol/refs?' + params.toString(), { cache: 'no-store' });
      if (!resp.ok) {
        throw new Error(`fetch failed: ${resp.status}`);
      }
      const data = await resp.json();
      const childRefs = (data && data.refs) || [];
      if (callerState && callerState.cache) {
        callerState.cache[declKey] = childRefs;
      }
      return childRefs;
    })().finally(() => {
      if (callerState && callerState.inflight) {
        delete callerState.inflight[declKey];
      }
    });

    if (callerState && callerState.inflight) {
      callerState.inflight[declKey] = fetchPromise;
    }
    return await fetchPromise;
  }

  async function fetchCallerRefsBatch(refs, stateCtx, callerState) {
    if (!refs || refs.length === 0 || !stateCtx) return [];
    const uncached = [];
    const seenKeys = new Set();
    const existingInflight = [];

    for (const ref of refs) {
      const k = getRefDeclKey(ref);
      if (!k || seenKeys.has(k)) continue;
      seenKeys.add(k);
      if (callerState && hasProp(callerState.cache, k)) continue;
      if (callerState && hasProp(callerState.inflight, k)) {
        existingInflight.push(callerState.inflight[k]);
        continue;
      }
      uncached.push({ key: k, ref });
    }

    if (existingInflight.length > 0) {
      await Promise.allSettled(existingInflight);
    }

    if (uncached.length === 0) return [];

    const CHUNK_SIZE = 64;
    for (let c = 0; c < uncached.length; c += CHUNK_SIZE) {
      const chunkItems = uncached.slice(c, c + CHUNK_SIZE);

      const deferreds = chunkItems.map(item => {
        let resolve, reject;
        const promise = new Promise((res, rej) => {
          resolve = res;
          reject = rej;
        });
        promise.catch(() => {});
        return { item, promise, resolve, reject };
      });

      if (callerState && callerState.inflight) {
        for (const def of deferreds) {
          callerState.inflight[def.item.key] = def.promise;
        }
      }

      const body = {
        repo: stateCtx.repo || '',
        change: stateCtx.change || '',
        side: stateCtx.side || 'R',
        left_rev: stateCtx.leftRev || '',
        right_rev: stateCtx.rightRev || '',
        base: stateCtx.base || '',
        target: stateCtx.target || '',
        requests: chunkItems.map(({ ref }) => ({
          file: ref.in_repo ? (ref.path || '') : (stateCtx.originPath || ''),
          line: ref.container_line,
          col: ref.container_utf16_col || 0,
          pkg: ref.in_repo ? '' : (ref.pkg || ''),
          pkg_file: ref.in_repo ? '' : (ref.pkg_file || ''),
        })),
      };

      try {
        const resp = await fetch('/api/symbol/refs/batch', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
          cache: 'no-store',
        });
        if (!resp.ok) {
          throw new Error(`batch fetch failed: ${resp.status}`);
        }
        const data = await resp.json();
        const results = (data && data.results) || [];
        for (let i = 0; i < deferreds.length; i++) {
          const def = deferreds[i];
          const res = (i < results.length) ? results[i] : null;
          const childRefs = (res && res.refs) || [];
          if (callerState && callerState.cache) {
            callerState.cache[def.item.key] = childRefs;
          }
          if (callerState && callerState.inflight) {
            delete callerState.inflight[def.item.key];
          }
          def.resolve(childRefs);
        }
      } catch (_) {
        if (callerState && callerState.inflight) {
          for (const def of deferreds) {
            delete callerState.inflight[def.item.key];
          }
        }
        await mapConcurrent(deferreds, 4, async (def) => {
          try {
            const res = await fetchCallerRefs(def.item.ref, stateCtx, callerState);
            def.resolve(res || []);
          } catch (e) {
            def.reject(e);
          }
        });
      } finally {
        if (callerState && callerState.inflight) {
          for (const def of deferreds) {
            delete callerState.inflight[def.item.key];
          }
        }
      }
    }
  }

  async function mapConcurrent(items, limit, fn) {
    const results = new Array(items.length);
    let idx = 0;
    async function worker() {
      while (idx < items.length) {
        const cur = idx++;
        results[cur] = await fn(items[cur]);
      }
    }
    const workers = [];
    for (let i = 0; i < Math.min(limit, items.length); i++) {
      workers.push(worker());
    }
    await Promise.all(workers);
    return results;
  }

  async function toggleCallerNode(inst, callerToggle) {
    if (!callerToggle || callerToggle.disabled || !inst || !inst.currentState) return;
    const contentEl = getPopupContentEl(inst);
    if (!contentEl) return;
    const callerState = inst.currentState.callerState;
    if (!callerState) return;

    const rowEl = callerToggle.closest('.sym-ref-row');
    const nodeId = callerToggle.getAttribute('data-node-id') || (rowEl ? rowEl.getAttribute('data-node-id') : '');
    if (!nodeId) return;

    let node = callerState.nodes[nodeId];
    if (!node) return;

    if (node.expanded) {
      node.expanded = false;
      const prefix = nodeId + '.';
      for (const id in callerState.nodes) {
        if (id.startsWith(prefix) && callerState.nodes[id].expanded) {
          callerState.nodes[id].expanded = false;
        }
      }
      callerToggle.innerHTML = CALLER_CHEVRON_RIGHT_SVG;
      callerToggle.setAttribute('aria-expanded', 'false');
      callerToggle.title = `Expand references to ${node.ref.container || ''}`;

      const childrenEl = contentEl.querySelector(`.sym-caller-children[data-parent-node-id="${nodeId}"]`);
      if (childrenEl) {
        if (childrenEl.remove) childrenEl.remove();
        else if (childrenEl.parentElement) childrenEl.parentElement.removeChild(childrenEl);
      }
      syncGroupButtons(contentEl, callerState, node.groupKey);
      if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
      return;
    }

    if (node.loaded) {
      node.expanded = true;
      callerToggle.innerHTML = CALLER_CHEVRON_DOWN_SVG;
      callerToggle.setAttribute('aria-expanded', 'true');
      callerToggle.title = `Collapse references to ${node.ref.container || ''}`;

      const existing = contentEl.querySelector(`.sym-caller-children[data-parent-node-id="${nodeId}"]`);
      if (existing) {
        if (existing.remove) existing.remove();
        else if (existing.parentElement) existing.parentElement.removeChild(existing);
      }

      const childrenEl = document.createElement('div');
      childrenEl.className = 'sym-caller-children';
      childrenEl.setAttribute('data-parent-node-id', nodeId);

      let childHtml = '';
      if (!node.childRefs || node.childRefs.length === 0) {
        childHtml = `<div class="sym-caller-empty" style="padding-left: ${10 + (node.depth + 1) * 16}px;">No references found</div>`;
      } else {
        for (let i = 0; i < node.childRefs.length; i++) {
          const cr = node.childRefs[i];
          const childNodeId = `${nodeId}.${i}`;
          if (!callerState.nodes[childNodeId]) {
            callerState.nodes[childNodeId] = {
              nodeId: childNodeId,
              ref: cr,
              depth: node.depth + 1,
              groupKey: node.groupKey,
              parentNodeId: nodeId,
              expanded: false,
              loaded: false,
              childRefs: [],
            };
          }
          childHtml += renderRefRowTreeHtml(cr, node.depth + 1, childNodeId, callerState);
        }
      }
      childrenEl.innerHTML = childHtml;
      if (rowEl.after) rowEl.after(childrenEl);
      else if (rowEl.parentElement) {
        const p = rowEl.parentElement;
        const idx = p.children.indexOf ? p.children.indexOf(rowEl) : -1;
        if (idx !== -1 && idx + 1 < p.children.length) {
          p.insertBefore(childrenEl, p.children[idx + 1]);
        } else {
          p.appendChild(childrenEl);
        }
      } else if (rowEl.parentNode && rowEl.parentNode.insertBefore) {
        rowEl.parentNode.insertBefore(childrenEl, rowEl.nextSibling);
      }
      syncGroupButtons(contentEl, callerState, node.groupKey);
      if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
      return;
    }

    callerToggle.disabled = true;
    callerToggle.textContent = '…';
    node.loading = true;

    const activeState = inst.currentState;
    try {
      const stateCtx = activeState.ctx;
      const childRefs = await fetchCallerRefs(node.ref, stateCtx, callerState);
      if (inst.currentState !== activeState) return;
      node.childRefs = childRefs || [];
      node.loaded = true;
      node.expanded = true;
      node.loading = false;
      callerToggle.disabled = false;
      callerToggle.innerHTML = CALLER_CHEVRON_DOWN_SVG;
      callerToggle.setAttribute('aria-expanded', 'true');
      callerToggle.title = `Collapse references to ${node.ref.container || ''}`;

      const childrenEl = document.createElement('div');
      childrenEl.className = 'sym-caller-children';
      childrenEl.setAttribute('data-parent-node-id', nodeId);

      let childHtml = '';
      if (!node.childRefs || node.childRefs.length === 0) {
        childHtml = `<div class="sym-caller-empty" style="padding-left: ${10 + (node.depth + 1) * 16}px;">No references found</div>`;
      } else {
        for (let i = 0; i < node.childRefs.length; i++) {
          const cr = node.childRefs[i];
          const childNodeId = `${nodeId}.${i}`;
          if (!callerState.nodes[childNodeId]) {
            callerState.nodes[childNodeId] = {
              nodeId: childNodeId,
              ref: cr,
              depth: node.depth + 1,
              groupKey: node.groupKey,
              parentNodeId: nodeId,
              expanded: false,
              loaded: false,
              childRefs: [],
            };
          }
          childHtml += renderRefRowTreeHtml(cr, node.depth + 1, childNodeId, callerState);
        }
      }
      childrenEl.innerHTML = childHtml;
      if (rowEl.after) rowEl.after(childrenEl);
      else if (rowEl.parentElement) {
        const p = rowEl.parentElement;
        const idx = p.children.indexOf ? p.children.indexOf(rowEl) : -1;
        if (idx !== -1 && idx + 1 < p.children.length) {
          p.insertBefore(childrenEl, p.children[idx + 1]);
        } else {
          p.appendChild(childrenEl);
        }
      } else if (rowEl.parentNode && rowEl.parentNode.insertBefore) {
        rowEl.parentNode.insertBefore(childrenEl, rowEl.nextSibling);
      }
      syncGroupButtons(contentEl, callerState, node.groupKey);
      if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
    } catch (err) {
      node.loading = false;
      if (inst && inst.currentState === activeState) {
        callerToggle.disabled = false;
        callerToggle.innerHTML = CALLER_CHEVRON_RIGHT_SVG;
      }
    }
  }

  async function handleExpandAllGroup(inst, groupKey) {
    if (!inst || !inst.currentState) return;
    const contentEl = getPopupContentEl(inst);
    if (!contentEl) return;
    const callerState = inst.currentState.callerState;
    if (!callerState) return;

    const nodesToExpand = getVisibleCollapsedExpandableNodes(groupKey, callerState);
    if (nodesToExpand.length === 0) return;

    callerState.groupLoading = callerState.groupLoading || {};
    callerState.groupLoading[groupKey] = true;
    syncGroupButtons(contentEl, callerState, groupKey);

    for (const node of nodesToExpand) {
      const rowEl = contentEl.querySelector(`.sym-ref-row[data-node-id="${node.nodeId}"]`);
      if (rowEl) {
        const toggle = rowEl.querySelector('.sym-caller-toggle');
        if (toggle) {
          toggle.textContent = '…';
          toggle.disabled = true;
        }
      }
      node.loading = true;
    }

    const activeState = inst.currentState;
    try {
      const stateCtx = activeState.ctx;
      const refsToFetch = nodesToExpand.filter(n => !n.loaded).map(n => n.ref);
      if (refsToFetch.length > 0) {
        await fetchCallerRefsBatch(refsToFetch, stateCtx, callerState);
      }
      await mapConcurrent(nodesToExpand, 6, async (node) => {
        if (!node.loaded) {
          const k = getRefDeclKey(node.ref);
          const childRefs = (callerState && hasProp(callerState.cache, k))
            ? callerState.cache[k]
            : await fetchCallerRefs(node.ref, stateCtx, callerState);
          node.childRefs = childRefs || [];
          node.loaded = true;
        }
        node.expanded = true;
        node.loading = false;
        if (node.childRefs) {
          for (let i = 0; i < node.childRefs.length; i++) {
            const cr = node.childRefs[i];
            const childNodeId = `${node.nodeId}.${i}`;
            if (!callerState.nodes[childNodeId]) {
              callerState.nodes[childNodeId] = {
                nodeId: childNodeId,
                ref: cr,
                depth: node.depth + 1,
                groupKey: node.groupKey,
                parentNodeId: node.nodeId,
                expanded: false,
                loaded: false,
                childRefs: [],
              };
            }
          }
        }
      });

      if (inst.currentState !== activeState) return;

      for (const node of nodesToExpand) {
        const rowEl = contentEl.querySelector(`.sym-ref-row[data-node-id="${node.nodeId}"]`);
        if (!rowEl) continue;
        const toggle = rowEl.querySelector('.sym-caller-toggle');
        if (toggle) {
          toggle.disabled = false;
          toggle.innerHTML = CALLER_CHEVRON_DOWN_SVG;
          toggle.setAttribute('aria-expanded', 'true');
          toggle.title = `Collapse references to ${node.ref.container || ''}`;
        }

        const existing = contentEl.querySelector(`.sym-caller-children[data-parent-node-id="${node.nodeId}"]`);
        if (existing) {
          if (existing.remove) existing.remove();
          else if (existing.parentElement) existing.parentElement.removeChild(existing);
        }

        const childrenEl = document.createElement('div');
        childrenEl.className = 'sym-caller-children';
        childrenEl.setAttribute('data-parent-node-id', node.nodeId);

        let childHtml = '';
        if (!node.childRefs || node.childRefs.length === 0) {
          childHtml = `<div class="sym-caller-empty" style="padding-left: ${10 + (node.depth + 1) * 16}px;">No references found</div>`;
        } else {
          for (let i = 0; i < node.childRefs.length; i++) {
            const cr = node.childRefs[i];
            const childNodeId = `${node.nodeId}.${i}`;
            childHtml += renderRefRowTreeHtml(cr, node.depth + 1, childNodeId, callerState);
          }
        }
        childrenEl.innerHTML = childHtml;
        if (rowEl.after) rowEl.after(childrenEl);
        else if (rowEl.parentElement) {
          const p = rowEl.parentElement;
          const idx = p.children.indexOf ? p.children.indexOf(rowEl) : -1;
          if (idx !== -1 && idx + 1 < p.children.length) {
            p.insertBefore(childrenEl, p.children[idx + 1]);
          } else {
            p.appendChild(childrenEl);
          }
        } else if (rowEl.parentNode && rowEl.parentNode.insertBefore) {
          rowEl.parentNode.insertBefore(childrenEl, rowEl.nextSibling);
        }
      }
    } catch (err) {
      if (inst && inst.currentState === activeState) {
        for (const node of nodesToExpand) {
          if (!node.expanded) {
            node.loading = false;
            const rowEl = contentEl.querySelector(`.sym-ref-row[data-node-id="${node.nodeId}"]`);
            if (rowEl) {
              const toggle = rowEl.querySelector('.sym-caller-toggle');
              if (toggle) {
                toggle.disabled = false;
                toggle.innerHTML = CALLER_CHEVRON_RIGHT_SVG;
              }
            }
          }
        }
      }
    } finally {
      callerState.groupLoading[groupKey] = false;
      if (inst && inst.currentState === activeState) {
        syncGroupButtons(contentEl, callerState, groupKey);
        if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
      }
    }
  }

  function handleCollapseAllGroup(inst, groupKey) {
    if (!inst || !inst.currentState) return;
    const contentEl = getPopupContentEl(inst);
    if (!contentEl) return;
    const callerState = inst.currentState.callerState;
    if (!callerState) return;

    const nodesToCollapse = [];
    for (const id in callerState.nodes) {
      const node = callerState.nodes[id];
      if (node.groupKey === groupKey && node.expanded && isNodeVisible(id, callerState) && !hasExpandedChild(id, callerState)) {
        nodesToCollapse.push(node);
      }
    }

    if (nodesToCollapse.length === 0) return;

    for (const node of nodesToCollapse) {
      node.expanded = false;
      const rowEl = contentEl.querySelector(`.sym-ref-row[data-node-id="${node.nodeId}"]`);
      if (rowEl) {
        const toggle = rowEl.querySelector('.sym-caller-toggle');
        if (toggle) {
          toggle.innerHTML = CALLER_CHEVRON_RIGHT_SVG;
          toggle.setAttribute('aria-expanded', 'false');
          toggle.title = `Expand references to ${node.ref.container || ''}`;
        }
        const childrenEl = contentEl.querySelector(`.sym-caller-children[data-parent-node-id="${node.nodeId}"]`);
        if (childrenEl) {
          if (childrenEl.remove) childrenEl.remove();
          else if (childrenEl.parentElement) childrenEl.parentElement.removeChild(childrenEl);
        }
      }
    }

    syncGroupButtons(contentEl, callerState, groupKey);
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  }

  function renderRefRowHtml(ref, depth = 0, nodeId = '', callerState = null) {
    if (!ref) return '';
    let targetAttr = '';
    if (ref.in_repo) {
      targetAttr = `data-path="${escapeHtml(ref.path || '')}"`;
    } else {
      targetAttr = `data-ext="true" data-pkg="${escapeHtml(ref.pkg || '')}" data-file="${escapeHtml(ref.pkg_file || '')}"`;
    }

    const upArrowHtml = (depth >= 1) ? CALLER_UP_ARROW_SVG : '';
    const expandable = canExpandRef(ref);

    let toggleBtn = '';
    if (expandable) {
      const node = (nodeId && callerState && callerState.nodes) ? callerState.nodes[nodeId] : null;
      const isExpanded = !!(node && node.expanded);
      const isLoading = !!(node && node.loading);
      const iconHtml = isLoading ? '…' : (isExpanded ? CALLER_CHEVRON_DOWN_SVG : CALLER_CHEVRON_RIGHT_SVG);
      const ariaExpanded = isExpanded ? 'true' : 'false';
      const title = `${isExpanded ? 'Collapse references' : 'Expand references'} to ${escapeHtml(ref.container || '')}`;
      toggleBtn = `<button type="button" class="sym-caller-toggle" data-node-id="${escapeHtml(nodeId)}" data-container-line="${ref.container_line}" data-container-col="${ref.container_utf16_col || 0}" ${targetAttr} title="${title}" aria-expanded="${ariaExpanded}">${iconHtml}</button>`;
    }

    let prefixHtml = '';
    if (upArrowHtml || toggleBtn) {
      prefixHtml = `<span class="sym-caller-prefix">${upArrowHtml}${toggleBtn}</span>`;
    }

    let nameHtml = '';
    if (ref.container) {
      nameHtml = `<span class="sym-ref-name">${prefixHtml}${escapeHtml(ref.container)}</span>`;
    } else if (prefixHtml) {
      nameHtml = `<span class="sym-ref-name">${prefixHtml}</span>`;
    }

    const locLabel = `${escapeHtml(ref.display || '')}:${ref.line}`;
    const locHtml = `<span class="sym-ref-loc">${locLabel}</span>`;
    const codeHtml = `<span class="sym-ref-code">${ref.line_html || ''}</span>`;
    const depthStyle = depth > 0 ? ` style="padding-left: ${10 + depth * 16}px;"` : '';
    const depthAttr = depth > 0 ? ` data-caller-depth="${depth}"` : '';
    const nodeAttr = nodeId ? ` data-node-id="${escapeHtml(nodeId)}"` : '';

    return `
      <div class="sym-ref-row" role="button" tabindex="0" data-resolve-sym="true" data-site="1" data-line="${ref.line}" ${targetAttr}${depthStyle}${depthAttr}${nodeAttr}>
        ${nameHtml}
        ${locHtml}
        ${codeHtml}
      </div>
    `;
  }

  function renderRefRowTreeHtml(ref, depth, nodeId, callerState) {
    let html = renderRefRowHtml(ref, depth, nodeId, callerState);
    const node = (callerState && callerState.nodes) ? callerState.nodes[nodeId] : null;
    if (node && node.expanded) {
      let childrenInner = '';
      if (!node.childRefs || node.childRefs.length === 0) {
        childrenInner = `<div class="sym-caller-empty" style="padding-left: ${10 + (depth + 1) * 16}px;">No references found</div>`;
      } else {
        for (let i = 0; i < node.childRefs.length; i++) {
          const childRef = node.childRefs[i];
          const childNodeId = `${nodeId}.${i}`;
          if (!callerState.nodes[childNodeId]) {
            callerState.nodes[childNodeId] = {
              nodeId: childNodeId,
              ref: childRef,
              depth: depth + 1,
              groupKey: node.groupKey,
              parentNodeId: nodeId,
              expanded: false,
              loaded: false,
              childRefs: [],
            };
          }
          childrenInner += renderRefRowTreeHtml(childRef, depth + 1, childNodeId, callerState);
        }
      }
      html += `<div class="sym-caller-children" data-parent-node-id="${escapeHtml(nodeId)}">${childrenInner}</div>`;
    }
    return html;
  }

  function renderRefsView(popupOrInst, refsData, stateCtx, restoredScrollTop, restoredScrollLeft) {
    const contentEl = getPopupContentEl(popupOrInst);
    if (!contentEl || !refsData) return;

    const targetInst = popupOrInst && popupOrInst.el ? popupOrInst : (openSymbolPopups.find(p => p && p.el === popupOrInst) || null);
    const currentState = targetInst && targetInst.currentState ? targetInst.currentState : null;

    let callerState;
    if (currentState && currentState.kind === 'refs') {
      if (!currentState.callerState) {
        currentState.callerState = { nodes: Object.create(null), cache: Object.create(null), inflight: Object.create(null), groupLoading: Object.create(null) };
      }
      callerState = currentState.callerState;
    } else {
      if (!refsData._callerState) {
        refsData._callerState = { nodes: Object.create(null), cache: Object.create(null), inflight: Object.create(null), groupLoading: Object.create(null) };
      }
      callerState = refsData._callerState;
    }

    const backBtnHtml = `<button type="button" class="sym-btn-back" title="Back" aria-label="Back">←</button>`;
    const totalCount = refsData.refs ? refsData.refs.length : 0;
    const badgeText = `${totalCount}${refsData.truncated ? '+' : ''}`;

    let bodyHtml = `
      <div class="sym-popup-header">
        ${backBtnHtml}
        <div class="sym-popup-loc">
          References to <code>${escapeHtml(refsData.symbol_name || '')}</code>
          <span class="sym-count-badge">${badgeText}</span>
        </div>
        <div class="sym-popup-actions">
          ${renderPopupWindowControlsHtml(popupOrInst)}
        </div>
      </div>
    `;

    if (refsData.signature_html) {
      bodyHtml += `<div class="sym-popup-sig">${refsData.signature_html}</div>`;
    } else if (refsData.signature) {
      bodyHtml += `<div class="sym-popup-sig">${escapeHtml(refsData.signature)}</div>`;
    }

    if (!refsData.refs || refsData.refs.length === 0) {
      bodyHtml += `<div class="sym-list-empty">No references found</div>`;
      contentEl.innerHTML = bodyHtml;
      syncPopupShadeUI(popupOrInst);
      ensurePopupMinWidth(popupOrInst);
      return;
    }

    const groups = [
      { key: 'change', title: 'In changed files' },
      { key: 'package', title: 'In package' },
      { key: 'repo', title: 'In repository' },
    ];

    let listHtml = '';
    for (const g of groups) {
      const gRefs = refsData.refs.filter(r => r.group === g.key);
      if (gRefs.length === 0) continue;

      const hasExpandable = gRefs.some(canExpandRef);
      let actionsHtml = '';
      if (hasExpandable) {
        actionsHtml = `
          <span class="sym-group-actions">
            <button type="button" class="sym-group-action-btn" data-caller-group-action="expand" data-ref-group="${g.key}">Expand All</button>
            <button type="button" class="sym-group-action-btn" data-caller-group-action="collapse" data-ref-group="${g.key}">Collapse All</button>
          </span>
        `;
      }
      listHtml += `
        <div class="sym-group-header" data-ref-group="${g.key}">
          <span class="sym-group-title">${escapeHtml(g.title)} (${gRefs.length})</span>
          ${actionsHtml}
        </div>
      `;

      for (let i = 0; i < gRefs.length; i++) {
        const ref = gRefs[i];
        const nodeId = `${g.key}:${i}`;
        if (!callerState.nodes[nodeId]) {
          callerState.nodes[nodeId] = {
            nodeId: nodeId,
            ref: ref,
            depth: 0,
            groupKey: g.key,
            parentNodeId: null,
            expanded: false,
            loaded: false,
            childRefs: [],
          };
        }
        listHtml += renderRefRowTreeHtml(ref, 0, nodeId, callerState);
      }
    }

    bodyHtml += `<div class="sym-list-body"><div class="sym-list-inner">${listHtml}</div></div>`;
    if (refsData.truncated) {
      bodyHtml += `<div class="sym-snippet-trunc">Reference search truncated at 200 matches</div>`;
    }

    contentEl.innerHTML = bodyHtml;
    syncPopupShadeUI(popupOrInst);
    ensurePopupMinWidth(popupOrInst);

    for (const g of groups) {
      syncGroupButtons(contentEl, callerState, g.key);
    }

    if (typeof restoredScrollTop === 'number') {
      const listBody = contentEl.querySelector('.sym-list-body');
      if (listBody) {
        listBody.scrollTop = restoredScrollTop;
      }
    }
    if (typeof restoredScrollLeft === 'number') {
      const listBody = contentEl.querySelector('.sym-list-body');
      if (listBody) {
        listBody.scrollLeft = restoredScrollLeft;
      }
    }
    if (targetInst && activePopupInst === targetInst && targetInst.el && targetInst.el.focus) {
      targetInst.el.focus({ preventScroll: true });
    }
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  }

  function renderSymbolPopup(popupOrInst, data, stateCtx, restoredScrollTop, canGoBack) {
    const contentEl = getPopupContentEl(popupOrInst);
    if (!contentEl || !data) return;

    const navCtx = getJumpNavContext();
    let defWillNav = false;
    if (data.in_repo && data.def_line > 0) {
      defWillNav = navCtx.willRepoNav(data.def_path, data.def_line);
    } else if (!data.in_repo && data.def_pkg && data.def_file && data.def_line > 0) {
      defWillNav = navCtx.willExtNav(data.def_pkg, data.def_file, data.def_line);
    }

    let headerLocHtml = '';
    if (data.display_path) {
      const locLabel = `${escapeHtml(data.display_path)}${data.def_line > 0 ? ':' + data.def_line : ''}`;
      const locInner = defWillNav
        ? `<span class="sym-loc-text">${locLabel}</span> <span class="sym-nav-arrow" aria-hidden="true">→</span>`
        : locLabel;
      const locClass = defWillNav ? 'sym-popup-loc-link sym-will-nav' : 'sym-popup-loc-link';
      const locTitle = defWillNav ? 'Open definition in single-file view' : 'Jump to definition';
      if (data.in_repo && data.def_line > 0) {
        const href = buildInRepoJumpHref(stateCtx, data.def_path, data.def_line);
        headerLocHtml = `<a href="${href}" class="${locClass}" data-jump-def data-def-path="${escapeHtml(data.def_path)}" data-def-line="${data.def_line}" data-lookup-side="${stateCtx.side}" title="${locTitle}">${locInner}</a>`;
      } else if (!data.in_repo && data.def_pkg && data.def_file && data.def_line > 0) {
        const extUrl = buildExtJumpHref(stateCtx, data.def_pkg, data.def_file, data.def_line, stateCtx.originPath);
        headerLocHtml = `<a href="${extUrl}" class="${locClass}" data-jump-def data-ext="true" data-pkg="${escapeHtml(data.def_pkg)}" data-file="${escapeHtml(data.def_file)}" data-def-line="${data.def_line}" data-lookup-side="${stateCtx.side}" title="${locTitle}">${locInner}</a>`;
      } else {
        headerLocHtml = `<span>${locLabel}</span>`;
      }
    } else {
      headerLocHtml = `<span>${escapeHtml(data.symbol_name || 'Symbol')}</span>`;
    }

    let actionsHtml = '';
    if (data.can_refs) {
      actionsHtml += `<button type="button" class="sym-btn-action sym-btn-refs" data-action="refs" title="Find references">Refs</button>`;
    }
    if (data.can_impls) {
      const implsTitle = data.impls_direction === 'interfaces' ? 'Find implemented interfaces' : 'Find implementations';
      actionsHtml += `<button type="button" class="sym-btn-action sym-btn-impls" data-action="impls" title="${implsTitle}">Impls</button>`;
    }
    if (data.pkg_go_dev_url) {
      actionsHtml += `<a href="${escapeHtml(data.pkg_go_dev_url)}" target="_blank" rel="noopener noreferrer" class="sym-btn-pkg">pkg.go.dev ↗</a>`;
    }
    actionsHtml += renderPopupWindowControlsHtml(popupOrInst);

    const backPossible = (typeof canGoBack === 'boolean') ? canGoBack :
      (popupOrInst && popupOrInst.history ? popupOrInst.history.length > 0 : false);
    const backBtnHtml = backPossible ? `<button type="button" class="sym-btn-back" title="Back" aria-label="Back">←</button>` : '';

    let headerInnerHtml = `
      ${backBtnHtml}
      <div class="sym-popup-loc">${headerLocHtml}</div>
      <div class="sym-popup-actions">${actionsHtml}</div>
    `;
    let bodyHtml = `
      <div class="sym-popup-header">
        ${headerInnerHtml}
      </div>
    `;

    if (data.signature_html) {
      bodyHtml += `<div class="sym-popup-sig">${data.signature_html}</div>`;
    } else if (data.signature) {
      bodyHtml += `<div class="sym-popup-sig">${escapeHtml(data.signature)}</div>`;
    }

    const hasSnippet = data.snippet && data.snippet.length > 0;
    if (hasSnippet) {
      let snippetRows = '';
      for (const sLine of data.snippet) {
        const activeClass = sLine.active ? ' sym-snippet-active' : '';
        snippetRows += `
          <tr class="sym-snippet-row${activeClass}" data-snippet-line="${sLine.line}">
            <td class="sym-snippet-num">${sLine.line}</td>
            <td class="sym-snippet-code">${sLine.html}</td>
          </tr>
        `;
      }
      bodyHtml += `
        <div class="sym-popup-snippet">
          <table class="sym-snippet-table"><tbody>${snippetRows}</tbody></table>
        </div>
      `;
      if (data.snippet_truncated) {
        bodyHtml += `<div class="sym-snippet-trunc">File exceeds 5,000 lines (truncated)</div>`;
      }
    }

    contentEl.innerHTML = bodyHtml;
    syncPopupShadeUI(popupOrInst);
    ensurePopupMinWidth(popupOrInst);

    // Handle scroll position in snippet
    const snippetEl = contentEl.querySelector ? contentEl.querySelector('.sym-popup-snippet') : null;
    if (snippetEl) {
      if (typeof restoredScrollTop === 'number') {
        snippetEl.scrollTop = restoredScrollTop;
      } else {
        const activeRow = contentEl.querySelector ? contentEl.querySelector('.sym-snippet-row.sym-snippet-active') : null;
        if (activeRow) {
          const rowRect = activeRow.getBoundingClientRect ? activeRow.getBoundingClientRect() : { top: 0 };
          const snipRect = snippetEl.getBoundingClientRect ? snippetEl.getBoundingClientRect() : { top: 0, height: 0 };
          let targetScroll = 0;
          if (snipRect.height > 0) {
            targetScroll = (rowRect.top - snipRect.top + snippetEl.scrollTop) - Math.floor(((snippetEl.clientHeight || 340) - (activeRow.offsetHeight || 20)) / 2);
          } else {
            targetScroll = (activeRow.offsetTop || 0) - Math.floor((340 - (activeRow.offsetHeight || 20)) / 2);
          }
          snippetEl.scrollTop = Math.max(0, targetScroll);
        }
      }
    }
    const targetInst = popupOrInst && popupOrInst.el ? popupOrInst : (openSymbolPopups.find(p => p && p.el === popupOrInst) || null);
    if (targetInst && targetInst.currentState) {
      targetInst.currentState.headerHtml = headerInnerHtml;
    }
    if (targetInst && activePopupInst === targetInst && targetInst.el && targetInst.el.focus) {
      targetInst.el.focus({ preventScroll: true });
    }
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  }

  function renderMovePopup(popupOrInst, data, stateCtx, restoredScrollTop, canGoBack) {
    const inst = (popupOrInst && popupOrInst.contentEl) ? popupOrInst :
      (openSymbolPopups.find(p => p && p.el === popupOrInst) || { el: popupOrInst, contentEl: (popupOrInst && popupOrInst.querySelector && popupOrInst.querySelector('.sym-popup-content')) || popupOrInst });
    const contentEl = getPopupContentEl(inst);
    if (!contentEl) return;

    const peerPath = (data.path || '').replace(/^\/+/, '');
    const peerSide = (data.side === 'L' ? 'L' : 'R');
    const prefix = peerSide === 'L' ? '← Moved from' : '→ Moved to';
    const rangeLabel = `L${data.start_line}${data.end_line > data.start_line ? '–' + data.end_line : ''}`;
    const fullLocLabel = `${peerPath}:${rangeLabel}`;

    const sp = new URLSearchParams(window.location.search);
    sp.set('side', peerSide);
    if (data.end_line > data.start_line) {
      sp.set('end', String(data.end_line));
    } else {
      sp.delete('end');
    }
    if (!sp.get('mode')) {
      const curMode = getCurrentPageDiffMode();
      if (curMode) sp.set('mode', curMode);
    }
    sp.delete('diffs');
    const cleanTarget = encodeURIComponent(peerPath).replace(/%2F/g, '/');
    const jumpHref = `/r/${encodeURIComponent(stateCtx.repo)}/c/${encodeURIComponent(stateCtx.change)}/f/${cleanTarget}?${sp.toString()}#L${data.start_line}`;

    const backPossible = (typeof canGoBack === 'boolean') ? canGoBack :
      (inst && inst.history ? inst.history.length > 0 : false);
    const backBtnHtml = backPossible ? `<button type="button" class="sym-btn-back" title="Back" aria-label="Back">←</button>` : '';

    const modLabelHtml = data.block_modified ? `<span class="sym-move-mod-label">modified</span>` : '';

    const navCtx = getJumpNavContext();
    const peerWillNav = navCtx.willRepoNav(peerPath, data.start_line);
    const locInner = peerWillNav
      ? `<span class="sym-loc-text">${escapeHtml(fullLocLabel)}</span> <span class="sym-nav-arrow" aria-hidden="true">→</span>`
      : escapeHtml(fullLocLabel);
    const locClass = peerWillNav ? 'sym-popup-loc-link sym-will-nav' : 'sym-popup-loc-link';
    const locTitle = peerWillNav ? 'Open moved code in single-file view' : 'Jump to moved code in diff';

    const headerLocHtml = `
      <span class="sym-move-prefix">${prefix}</span>
      <a href="${jumpHref}" class="${locClass}" data-jump-move-peer="true" data-peer-path="${escapeHtml(peerPath)}" data-peer-start="${data.start_line}" data-peer-end="${data.end_line || data.start_line}" data-peer-side="${peerSide}" title="${locTitle}">${locInner}</a>
      ${modLabelHtml}
    `;

    const actionsHtml = renderPopupWindowControlsHtml(inst);

    let headerInnerHtml = `
      ${backBtnHtml}
      <div class="sym-popup-loc">${headerLocHtml}</div>
      <div class="sym-popup-actions">${actionsHtml}</div>
    `;

    let bodyHtml = `
      <div class="sym-popup-header">
        ${headerInnerHtml}
      </div>
    `;

    if (data.rows && data.rows.length > 0) {
      let snippetRows = '';
      for (const row of data.rows) {
        let rowClass = 'sym-snippet-row';
        if (row.moved) {
          rowClass += ' sym-move-row';
          if (row.modified) {
            rowClass += (peerSide === 'L' ? ' sym-move-row-del' : ' sym-move-row-add');
          }
        }
        snippetRows += `
          <tr class="${rowClass}" data-snippet-line="${row.line}">
            <td class="sym-snippet-num">${row.line}</td>
            <td class="sym-snippet-code">${row.html}</td>
          </tr>
        `;
      }
      bodyHtml += `
        <div class="sym-popup-snippet">
          <table class="sym-snippet-table"><tbody>${snippetRows}</tbody></table>
        </div>
      `;
    }

    contentEl.innerHTML = bodyHtml;
    syncPopupShadeUI(inst);
    ensurePopupMinWidth(inst);

    const snippetEl = contentEl.querySelector ? contentEl.querySelector('.sym-popup-snippet') : null;
    if (snippetEl) {
      if (typeof restoredScrollTop === 'number') {
        snippetEl.scrollTop = restoredScrollTop;
      } else {
        snippetEl.scrollTop = 0;
      }
    }

    const targetInst = inst && inst.el ? inst : (openSymbolPopups.find(p => p && p.el === inst) || null);
    if (targetInst && targetInst.currentState) {
      targetInst.currentState.headerHtml = headerInnerHtml;
    }
    if (targetInst && activePopupInst === targetInst && targetInst.el && targetInst.el.focus) {
      targetInst.el.focus({ preventScroll: true });
    }
    if (typeof updateIdentOccurrences === 'function') updateIdentOccurrences();
  }

  function attachPopupClickHandler(inst) {
    if (!inst || !inst.el) return;
    const popup = inst.el;

    if (!popup._shadeDblClickBound && popup.addEventListener) {
      popup._shadeDblClickBound = true;
      popup.addEventListener('dblclick', (evt) => {
        if (!evt || !evt.target) return;
        const header = evt.target.closest ? evt.target.closest('.sym-popup-header') : null;
        if (!header || (header.closest && header.closest('.symbol-popup') !== popup)) return;
        if (evt.target.closest && evt.target.closest('button, a, input, select, textarea')) return;
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        togglePopupShade(inst);
      });
    }

    popup.addEventListener('click', async (evt) => {
      if (!evt || !evt.target) return;
      if (inst._suppressNextLinkClick) {
        const locLink = evt.target.closest ? evt.target.closest('a.sym-popup-loc-link') : null;
        if (locLink) {
          if (evt.preventDefault) evt.preventDefault();
          if (evt.stopPropagation) evt.stopPropagation();
          inst._suppressNextLinkClick = false;
          return;
        }
      }

      const shadeBtn = evt.target.closest ? evt.target.closest('.sym-btn-shade') : null;
      if (shadeBtn) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        togglePopupShade(inst);
        return;
      }

      const closeBtn = evt.target.closest ? evt.target.closest('.sym-btn-close') : null;
      if (closeBtn) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        closeSpecificSymbolPopup(inst);
        return;
      }

      const backBtn = evt.target.closest ? evt.target.closest('.sym-btn-back') : null;
      if (backBtn) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        stepPopupBackOrCancel(inst);
        return;
      }

      const refsBtn = evt.target.closest ? evt.target.closest('.sym-btn-refs, [data-action="refs"]') : null;
      if (refsBtn) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        if (!inst.currentState || !inst.currentState.lookupParams) return;
        if (inst.shaded) {
          setPopupShaded(inst, false, false);
        }

        if (inst._abortPendingNav) {
          inst._abortPendingNav();
          inst._abortPendingNav = null;
        } else if (inst.listAbort) {
          inst.listAbort.abort();
          inst.listAbort = null;
        }

        const contentEl = getPopupContentEl(inst);
        const snippetEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-snippet') : null;
        const baseState = inst.currentState;
        baseState.scrollTop = snippetEl ? snippetEl.scrollTop : (baseState.scrollTop || 0);

        const stateCtx = baseState.ctx;
        const curData = baseState.data;
        const params = baseState.lookupParams;

        const headerEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-header') : null;
        const prevHeaderHtml = (baseState && baseState.headerHtml) || (headerEl ? headerEl.innerHTML : null);
        const prevContentHtml = !headerEl && contentEl ? contentEl.innerHTML : null;

        const loadingHeaderHtml = `
          <button type="button" class="sym-btn-back" title="Back" aria-label="Back">←</button>
          <div class="sym-popup-loc" style="color: var(--muted); font-size: 11px;">Finding references…</div>
          <div class="sym-popup-actions">
            ${renderPopupWindowControlsHtml(inst)}
          </div>
        `;

        if (headerEl) {
          headerEl.innerHTML = loadingHeaderHtml;
        } else if (contentEl) {
          contentEl.innerHTML = `<div class="sym-popup-header">${loadingHeaderHtml}</div>`;
        }
        syncPopupShadeUI(inst);

        function restoreHeader() {
          if (headerEl && prevHeaderHtml !== null) {
            headerEl.innerHTML = prevHeaderHtml;
          } else if (contentEl && prevContentHtml !== null) {
            contentEl.innerHTML = prevContentHtml;
          }
          syncPopupShadeUI(inst);
        }

        inst.listAbort = new AbortController();
        inst._abortPendingNav = () => {
          if (inst.listAbort) {
            inst.listAbort.abort();
            inst.listAbort = null;
          }
          restoreHeader();
        };

        try {
          const resp = await fetch('/api/symbol/refs?' + params.toString(), {
            cache: 'no-store',
            signal: inst.listAbort.signal,
          });
          inst.listAbort = null;
          inst._abortPendingNav = null;
          if (!resp.ok) {
            restoreHeader();
            return;
          }
          const refsData = await resp.json();
          if (!refsData || !refsData.found) {
            restoreHeader();
            return;
          }
          inst.history.push(baseState);
          inst.currentState = {
            kind: 'refs',
            data: refsData,
            ctx: stateCtx,
            lookupParams: params,
            scrollTop: 0,
          };
          renderRefsView(inst, refsData, stateCtx);
        } catch (err) {
          inst._abortPendingNav = null;
          if (err && err.name === 'AbortError') return;
          restoreHeader();
        }
        return;
      }

      const implsBtn = evt.target.closest ? evt.target.closest('.sym-btn-impls, [data-action="impls"]') : null;
      if (implsBtn) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        if (!inst.currentState || !inst.currentState.lookupParams) return;
        if (inst.shaded) {
          setPopupShaded(inst, false, false);
        }

        if (inst._abortPendingNav) {
          inst._abortPendingNav();
          inst._abortPendingNav = null;
        } else if (inst.listAbort) {
          inst.listAbort.abort();
          inst.listAbort = null;
        }

        const contentEl = getPopupContentEl(inst);
        const snippetEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-snippet') : null;
        const baseState = inst.currentState;
        baseState.scrollTop = snippetEl ? snippetEl.scrollTop : (baseState.scrollTop || 0);

        const stateCtx = baseState.ctx;
        const curData = baseState.data;
        const params = baseState.lookupParams;

        const headerEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-header') : null;
        const prevHeaderHtml = (baseState && baseState.headerHtml) || (headerEl ? headerEl.innerHTML : null);
        const prevContentHtml = !headerEl && contentEl ? contentEl.innerHTML : null;

        const loadingText = curData && curData.impls_direction === 'interfaces' ? 'Finding interfaces…' : 'Finding implementations…';
        const loadingHeaderHtml = `
          <button type="button" class="sym-btn-back" title="Back" aria-label="Back">←</button>
          <div class="sym-popup-loc" style="color: var(--muted); font-size: 11px;">${loadingText}</div>
          <div class="sym-popup-actions">
            ${renderPopupWindowControlsHtml(inst)}
          </div>
        `;

        if (headerEl) {
          headerEl.innerHTML = loadingHeaderHtml;
        } else if (contentEl) {
          contentEl.innerHTML = `<div class="sym-popup-header">${loadingHeaderHtml}</div>`;
        }
        syncPopupShadeUI(inst);

        function restoreHeader() {
          if (headerEl && prevHeaderHtml !== null) {
            headerEl.innerHTML = prevHeaderHtml;
          } else if (contentEl && prevContentHtml !== null) {
            contentEl.innerHTML = prevContentHtml;
          }
          syncPopupShadeUI(inst);
        }

        inst.listAbort = new AbortController();
        inst._abortPendingNav = () => {
          if (inst.listAbort) {
            inst.listAbort.abort();
            inst.listAbort = null;
          }
          restoreHeader();
        };

        try {
          const resp = await fetch('/api/symbol/impls?' + params.toString(), {
            cache: 'no-store',
            signal: inst.listAbort.signal,
          });
          inst.listAbort = null;
          inst._abortPendingNav = null;
          if (!resp.ok) {
            restoreHeader();
            return;
          }
          const implsData = await resp.json();
          if (!implsData || !implsData.found) {
            restoreHeader();
            return;
          }
          inst.history.push(baseState);
          inst.currentState = {
            kind: 'impls',
            data: implsData,
            ctx: stateCtx,
            lookupParams: params,
            scrollTop: 0,
          };
          renderImplsView(inst, implsData, stateCtx);
        } catch (err) {
          inst._abortPendingNav = null;
          if (err && err.name === 'AbortError') return;
          restoreHeader();
        }
        return;
      }

      const groupActionBtn = evt.target.closest ? evt.target.closest('[data-caller-group-action]') : null;
      if (groupActionBtn) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        if (groupActionBtn.disabled) return;
        const action = groupActionBtn.getAttribute('data-caller-group-action');
        const groupKey = groupActionBtn.getAttribute('data-ref-group');
        if (action === 'expand') {
          await handleExpandAllGroup(inst, groupKey);
        } else if (action === 'collapse') {
          handleCollapseAllGroup(inst, groupKey);
        }
        return;
      }

      const callerPrefix = evt.target.closest ? evt.target.closest('.sym-caller-prefix') : null;
      const callerToggle = (evt.target.closest ? evt.target.closest('.sym-caller-toggle') : null) || (callerPrefix ? callerPrefix.querySelector('.sym-caller-toggle') : null);
      if (callerPrefix || callerToggle) {
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        if (callerToggle && !callerToggle.disabled) {
          await toggleCallerNode(inst, callerToggle);
        }
        return;
      }

      const resolveBtn = evt.target.closest ? evt.target.closest('[data-resolve-sym]') : null;
      if (resolveBtn) {
        const sel = window.getSelection ? window.getSelection() : null;
        if (sel && !sel.isCollapsed) {
          return;
        }
        if (evt.preventDefault) evt.preventDefault();
        if (evt.stopPropagation) evt.stopPropagation();
        if (!inst.currentState) return;

        const baseState = inst.currentState;
        const contentEl = getPopupContentEl(inst);
        const listBody = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-list-body') : null;
        if (listBody) {
          baseState.scrollTop = listBody.scrollTop;
          baseState.scrollLeft = listBody.scrollLeft;
        }

        const stateCtx = baseState.ctx;
        const line = parseInt(resolveBtn.getAttribute('data-line') || '0', 10);
        const col = parseInt(resolveBtn.getAttribute('data-col') || '0', 10);
        const isSite = resolveBtn.getAttribute('data-site') === '1';
        const isExt = resolveBtn.getAttribute('data-ext') === 'true';
        const pPath = resolveBtn.getAttribute('data-path') || '';
        const pPkg = resolveBtn.getAttribute('data-pkg') || '';
        const pFile = resolveBtn.getAttribute('data-file') || '';

        const chParams = new URLSearchParams({
          repo: stateCtx.repo,
          change: stateCtx.change,
          side: stateCtx.side,
          line: line,
          col: col,
          left_rev: stateCtx.leftRev,
          right_rev: stateCtx.rightRev,
          base: stateCtx.base,
          target: stateCtx.target,
        });

        if (isSite) {
          chParams.set('site', '1');
        }

        if (isExt) {
          chParams.set('pkg', pPkg);
          chParams.set('pkg_file', pFile);
          if (stateCtx.originPath) {
            chParams.set('path', stateCtx.originPath);
          }
        } else if (pPath) {
          chParams.set('path', pPath);
        }

        const locEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-loc') : null;
        const oldLocHtml = locEl ? locEl.innerHTML : '';
        if (locEl) locEl.innerHTML = `<span style="color: var(--muted);">Resolving …</span>`;

        if (inst._abortPendingNav) {
          inst._abortPendingNav();
          inst._abortPendingNav = null;
        } else if (inst.listAbort) {
          inst.listAbort.abort();
          inst.listAbort = null;
        }
        inst.listAbort = new AbortController();
        inst._abortPendingNav = () => {
          if (inst.listAbort) {
            inst.listAbort.abort();
            inst.listAbort = null;
          }
          if (locEl) locEl.innerHTML = oldLocHtml;
        };

        try {
          const resp = await fetch('/api/symbol?' + chParams.toString(), {
            cache: 'no-store',
            signal: inst.listAbort.signal,
          });
          inst.listAbort = null;
          inst._abortPendingNav = null;
          if (!resp.ok) {
            if (locEl) locEl.innerHTML = oldLocHtml;
            return;
          }
          const chainedData = await resp.json();
          if (!chainedData || !chainedData.found) {
            if (locEl) {
              locEl.innerHTML = `<span style="color: var(--muted);">(not found)</span>`;
              setTimeout(() => { if (locEl && inst.currentState === baseState) locEl.innerHTML = oldLocHtml; }, 1500);
            }
            return;
          }

          inst.history.push(baseState);

          const nextParams = new URLSearchParams(chParams);
          if (isSite) {
            nextParams.delete('site');
            if (chainedData.sym_line > 0) {
              nextParams.set('line', chainedData.sym_line);
              nextParams.set('col', chainedData.sym_col || 0);
            }
          }

          inst.currentState = {
            kind: 'def',
            data: chainedData,
            ctx: stateCtx,
            lookupParams: nextParams,
            scrollTop: 0,
          };
          renderSymbolPopup(inst, chainedData, stateCtx, undefined, true);
        } catch (err) {
          inst._abortPendingNav = null;
          if (err && err.name === 'AbortError') return;
          if (locEl) locEl.innerHTML = oldLocHtml;
        }
        return;
      }

      const moveJumpLink = evt.target.closest ? evt.target.closest('[data-jump-move-peer]') : null;
      if (moveJumpLink) {
        const sel = window.getSelection ? window.getSelection() : null;
        if (sel && !sel.isCollapsed) {
          if (evt.preventDefault) evt.preventDefault();
          return;
        }
        const targetPath = moveJumpLink.getAttribute('data-peer-path') || '';
        const targetStart = parseInt(moveJumpLink.getAttribute('data-peer-start') || '0', 10);
        const targetEnd = parseInt(moveJumpLink.getAttribute('data-peer-end') || '0', 10);
        const targetSide = moveJumpLink.getAttribute('data-peer-side') || 'R';
        const curCtx = (inst.currentState && inst.currentState.ctx) || {};
        (window.jumpToMovePeerTarget || jumpToMovePeerTarget)({
          repo: curCtx.repo,
          change: curCtx.change,
          peerPath: targetPath,
          peerStart: targetStart,
          peerEnd: targetEnd,
          peerSide: targetSide,
        }, evt);
        return;
      }

      const jumpBtn = evt.target.closest ? evt.target.closest('[data-jump-def]') : null;
      if (jumpBtn) {
        const sel = window.getSelection ? window.getSelection() : null;
        if (sel && !sel.isCollapsed) {
          if (evt.preventDefault) evt.preventDefault();
          return;
        }
        const isExt = jumpBtn.getAttribute('data-ext') === 'true';
        if (isExt) {
          const pkg = jumpBtn.getAttribute('data-pkg');
          const file = jumpBtn.getAttribute('data-file');
          const l = parseInt(jumpBtn.getAttribute('data-def-line') || '0', 10);
          const s = jumpBtn.getAttribute('data-lookup-side') || 'R';
          const curCtx = (inst.currentState && inst.currentState.ctx) || {};
          handleJumpToExternalDefinition(curCtx.repo, curCtx.change, pkg, file, l, s, evt, curCtx.originPath);
          return;
        }
        const p = jumpBtn.getAttribute('data-def-path');
        const l = parseInt(jumpBtn.getAttribute('data-def-line') || '0', 10);
        const s = jumpBtn.getAttribute('data-lookup-side') || 'R';
        const curCtx = (inst.currentState && inst.currentState.ctx) || {};
        handleJumpToDefinition(curCtx.repo, curCtx.change, p, l, s, evt);
        return;
      }

      if (evt.button !== 0 || evt.metaKey || evt.ctrlKey || evt.altKey || evt.shiftKey || evt.detail !== 1) {
        return;
      }
      const sel = window.getSelection ? window.getSelection() : null;
      if (sel && !sel.isCollapsed) {
        return;
      }

      const chTok = evt.target.closest ? evt.target.closest('.tok-id, .tok-typ, .tok-imp, .tok-doc') : null;
      if (!chTok) return;
      const chCodeTd = chTok.closest ? chTok.closest('.sym-snippet-code') : null;
      if (!chCodeTd) return;
      const chTr = chCodeTd.closest ? chCodeTd.closest('tr.sym-snippet-row') : null;
      if (!chTr) return;
      const chLine = parseInt((chTr.getAttribute && chTr.getAttribute('data-snippet-line')) || '0', 10);
      if (chLine <= 0) return;
      const chCol = getCharOffsetInElement(chCodeTd, chTok, 0);

      if (!inst.currentState) return;

      if (evt.preventDefault) evt.preventDefault();
      if (evt.stopPropagation) evt.stopPropagation();

      const baseState = inst.currentState;
      const contentEl = getPopupContentEl(inst);
      const snippetEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-snippet') : null;
      if (snippetEl) {
        baseState.scrollTop = snippetEl.scrollTop;
      }

      const stateCtx = baseState.ctx;
      const prevData = baseState.data;

      const chParams = new URLSearchParams({
        repo: stateCtx.repo,
        change: stateCtx.change,
        side: stateCtx.side,
        line: chLine,
        col: chCol,
        left_rev: stateCtx.leftRev,
        right_rev: stateCtx.rightRev,
        base: stateCtx.base,
        target: stateCtx.target,
      });

      if (prevData.in_repo && prevData.def_path) {
        chParams.set('path', prevData.def_path);
      } else if (prevData.def_pkg) {
        chParams.set('pkg', prevData.def_pkg);
        chParams.set('pkg_file', prevData.def_file || prevData.def_path || '');
        if (stateCtx.originPath) {
          chParams.set('path', stateCtx.originPath);
        }
      } else {
        chParams.set('path', prevData.def_path || '');
      }

      const locEl = contentEl && contentEl.querySelector ? contentEl.querySelector('.sym-popup-loc') : null;
      if (baseState.baseLocHtml === undefined && locEl) {
        baseState.baseLocHtml = locEl.innerHTML;
      }
      const oldLocHtml = baseState.baseLocHtml || (locEl ? locEl.innerHTML : '');
      if (locEl) locEl.innerHTML = `<span style="color: var(--muted);">Resolving ${escapeHtml(chTok.textContent || '')}…</span>`;

      if (inst._abortPendingNav) {
        inst._abortPendingNav();
        inst._abortPendingNav = null;
      } else if (inst.listAbort) {
        inst.listAbort.abort();
        inst.listAbort = null;
      }
      inst.listAbort = new AbortController();
      inst._abortPendingNav = () => {
        if (inst.listAbort) {
          inst.listAbort.abort();
          inst.listAbort = null;
        }
        if (locEl) locEl.innerHTML = oldLocHtml;
      };

      try {
        const resp = await fetch('/api/symbol?' + chParams.toString(), {
          cache: 'no-store',
          signal: inst.listAbort.signal,
        });
        inst.listAbort = null;
        inst._abortPendingNav = null;
        if (!resp.ok) {
          if (locEl) locEl.innerHTML = oldLocHtml;
          return;
        }
        const chainedData = await resp.json();
        if (!chainedData || !chainedData.found) {
          if (locEl) {
            locEl.innerHTML = `<span style="color: var(--muted);">${escapeHtml(chTok.textContent || '')} (not found)</span>`;
            setTimeout(() => { if (locEl && inst.currentState && inst.currentState.data === prevData) locEl.innerHTML = oldLocHtml; }, 1500);
          }
          return;
        }

        inst.history.push(baseState);
        inst.currentState = {
          kind: 'def',
          data: chainedData,
          ctx: stateCtx,
          lookupParams: chParams,
          scrollTop: 0,
        };
        renderSymbolPopup(inst, chainedData, stateCtx, undefined, true);
      } catch (err) {
        inst._abortPendingNav = null;
        if (err && err.name === 'AbortError') return;
        if (locEl) locEl.innerHTML = oldLocHtml;
      }
    });

    popup.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        if (e.target.closest && (e.target.closest('.sym-caller-toggle') || e.target.closest('.sym-group-action-btn') || e.target.closest('.sym-caller-prefix'))) {
          return;
        }
        const resolveBtn = e.target.closest ? e.target.closest('[data-resolve-sym]') : null;
        if (resolveBtn) {
          if (e.preventDefault) e.preventDefault();
          resolveBtn.click();
        }
      }
    });
  }

  function createPopupInstance(anchorEl, highlightAnchor, initialLocText) {
    dismissUnpinnedPopupsExcept(null);
    if (highlightAnchor && anchorEl && anchorEl.classList && anchorEl.classList.add) {
      anchorEl.classList.add('sym-active');
    }

    const instId = `sym-popup-${++popupIdCounter}`;
    const lookupAbort = new AbortController();

    const popup = document.createElement('div');
    popup.className = 'symbol-popup';
    popup.id = popup.appendChild ? instId : 'symbolPopup';
    if (popup.setAttribute) {
      popup.setAttribute('data-popup-id', instId);
      popup.setAttribute('tabindex', '-1');
    }

    // 8 resize handles
    const resizeDirs = ['n', 's', 'e', 'w', 'nw', 'ne', 'sw', 'se'];
    for (const dir of resizeDirs) {
      const handle = document.createElement('div');
      handle.className = `sym-resize-handle sym-resize-${dir}`;
      if (handle.setAttribute) handle.setAttribute('data-resize-dir', dir);
      if (popup.appendChild) popup.appendChild(handle);
    }

    const contentEl = document.createElement('div');
    contentEl.className = 'sym-popup-content';
    contentEl.innerHTML = `
      <div class="sym-popup-header">
        <div class="sym-popup-loc" style="color: var(--muted); font-size: 11px;">${escapeHtml(initialLocText || 'Loading…')}</div>
        <div class="sym-popup-actions">
          ${renderPopupWindowControlsHtml(null)}
        </div>
      </div>
    `;
    if (popup.appendChild) popup.appendChild(contentEl);
    popup.contentEl = contentEl;
    if (!contentEl.parentElement && popup.innerHTML !== undefined && !popup.appendChild) {
      popup.innerHTML = contentEl.innerHTML;
    }

    const inst = {
      id: instId,
      el: popup,
      contentEl: contentEl,
      token: highlightAnchor ? anchorEl : null,
      lookupAbort: lookupAbort,
      listAbort: null,
      history: [],
      currentState: null,
      moved: false,
      resizedW: false,
      resizedH: false,
      shaded: false,
      usedShade: false,
      _suppressNextLinkClick: false,
    };

    bringPopupToFront(inst);
    openSymbolPopups.push(inst);
    if (popup.addEventListener) {
      popup.addEventListener('focusin', () => {
        activePopupInst = inst;
      });
    }

    setupDragAndResize(inst);
    attachPopupClickHandler(inst);

    if (document.body && document.body.appendChild) {
      document.body.appendChild(popup);
    }
    positionSymbolPopup(inst, anchorEl);

    return inst;
  }

  window.openSymbolPopupAt = openSymbolPopupAt;
  async function openSymbolPopupAt(container, side, line, col, anchorEl, highlightAnchor) {
    if (!container || line <= 0 || !anchorEl) return false;
    const filePath = (container.getAttribute && container.getAttribute('data-file-path')) || '';
    const extPkg = (container.getAttribute && container.getAttribute('data-ext-pkg')) || '';
    const extFile = (container.getAttribute && container.getAttribute('data-ext-file')) || '';
    const originPath = (container.getAttribute && container.getAttribute('data-origin-path')) || '';
    const isGo = (extPkg && extFile) ? extFile.endsWith('.go') : filePath.endsWith('.go');
    if (!isGo) return false;

    const inst = createPopupInstance(anchorEl, highlightAnchor, 'Inspecting symbol…');
    const signal = inst.lookupAbort.signal;

    const allFilesEl = document.querySelector('.diff-all-files');
    const repo = (container.getAttribute && container.getAttribute('data-repo')) || (allFilesEl && allFilesEl.getAttribute && allFilesEl.getAttribute('data-repo')) || '';
    const change = (container.getAttribute && container.getAttribute('data-change')) || (allFilesEl && allFilesEl.getAttribute && allFilesEl.getAttribute('data-change')) || '';
    const leftRev = (container.getAttribute && container.getAttribute('data-left-rev')) || '';
    const rightRev = (container.getAttribute && container.getAttribute('data-right-rev')) || '';
    const sp = new URLSearchParams(window.location.search);
    const base = sp.get('base') || '';
    const target = sp.get('target') || '';

    const params = new URLSearchParams({
      repo: repo,
      change: change,
      side: side,
      line: line,
      col: col,
      left_rev: leftRev,
      right_rev: rightRev,
      base: base,
      target: target,
    });
    if (extPkg && extFile) {
      params.set('pkg', extPkg);
      params.set('pkg_file', extFile);
      if (originPath) {
        params.set('path', originPath);
      }
    } else {
      params.set('path', filePath);
    }

    try {
      const resp = await fetch('/api/symbol?' + params.toString(), { signal, cache: 'no-store' });
      if (signal.aborted) return true;
      if (!resp.ok) {
        const errJson = await resp.json().catch(() => null);
        const errMsg = (errJson && errJson.error) || `HTTP error ${resp.status}`;
        const cEl = getPopupContentEl(inst);
        if (cEl) {
          cEl.innerHTML = `
            <div class="sym-popup-header">
              <div class="sym-popup-loc" style="color: var(--danger);">Error</div>
              <div class="sym-popup-actions">
                ${renderPopupWindowControlsHtml(inst)}
              </div>
            </div>
            <div style="padding: 10px; color: var(--danger); font-size: 11px;">${escapeHtml(errMsg)}</div>
          `;
          syncPopupShadeUI(inst);
        }
        if (!inst.moved && !inst.resizedW && !inst.resizedH) {
          positionSymbolPopup(inst, anchorEl);
        }
        return true;
      }

      const data = await resp.json();
      if (signal.aborted) return true;
      if (!data || !data.found) {
        closeSpecificSymbolPopup(inst);
        return false;
      }

      const stateCtx = {
        repo: repo,
        change: change,
        leftRev: leftRev,
        rightRev: rightRev,
        base: base,
        target: target,
        side: side,
        mode: getCurrentPageDiffMode(),
        originPath: originPath || (extPkg ? '' : filePath),
      };

      inst.currentState = {
        kind: 'def',
        data: data,
        ctx: stateCtx,
        lookupParams: params,
        scrollTop: 0,
      };

      renderSymbolPopup(inst, data, stateCtx);
      if (!inst.moved && !inst.resizedW && !inst.resizedH) {
        positionSymbolPopup(inst, anchorEl);
      }
      return true;
    } catch (err) {
      if (signal.aborted) return true;
      closeSpecificSymbolPopup(inst);
      return false;
    }
  }

  async function handleOpenMovePeerPopup(e, btnEl) {
    if (!btnEl) return;
    if (e) {
      if (e.button !== 0 || e.shiftKey || e.altKey || e.ctrlKey || e.metaKey) return;
      if (e.preventDefault) e.preventDefault();
      if (e.stopPropagation) e.stopPropagation();
    }

    const container = btnEl.closest ? btnEl.closest('.diff-container') : null;
    const allFilesEl = document.querySelector ? document.querySelector('.diff-all-files') : null;
    const currentPath = container ? (container.getAttribute('data-file-path') || '').replace(/^\/+/, '') : '';

    const actionsWrap = (btnEl.closest && btnEl.closest('.move-header-actions')) || btnEl.parentElement;
    const siblingBadge = actionsWrap && actionsWrap.querySelector ? actionsWrap.querySelector('.move-badge') : null;

    let peerPath = (btnEl.getAttribute('data-peer-path') || (siblingBadge && siblingBadge.getAttribute('data-peer-path')) || '').replace(/^\/+/, '');
    if (!peerPath) {
      peerPath = currentPath;
    }
    const peerStart = parseInt(btnEl.getAttribute('data-peer-start') || (siblingBadge && siblingBadge.getAttribute('data-peer-start')) || '0', 10);
    let peerEnd = parseInt(btnEl.getAttribute('data-peer-end') || (siblingBadge && siblingBadge.getAttribute('data-peer-end')) || '0', 10);
    if (peerEnd < peerStart) {
      peerEnd = peerStart;
    }
    const peerSide = btnEl.getAttribute('data-peer-side') || (siblingBadge && siblingBadge.getAttribute('data-peer-side')) || 'R';

    if (!peerPath || !(peerStart > 0)) {
      return false;
    }

    const repo = (container && container.getAttribute('data-repo')) || (allFilesEl && allFilesEl.getAttribute('data-repo')) || '';
    const change = (container && container.getAttribute('data-change')) || (allFilesEl && allFilesEl.getAttribute('data-change')) || '';
    const sp = new URLSearchParams(window.location.search);
    const base = sp.get('base') || '';
    const target = sp.get('target') || '';

    const inst = createPopupInstance(btnEl, false, 'Loading moved block…');
    const signal = inst.lookupAbort.signal;

    const params = new URLSearchParams({
      repo: repo,
      change: change,
      base: base,
      target: target,
      path: peerPath,
      side: peerSide,
      start: peerStart,
      end: peerEnd,
    });

    try {
      const resp = await fetch('/api/diff/move-preview?' + params.toString(), { signal, cache: 'no-store' });
      if (signal.aborted) return true;
      if (!resp.ok) {
        const cEl = getPopupContentEl(inst);
        if (cEl) {
          cEl.innerHTML = `
            <div class="sym-popup-header">
              <div class="sym-popup-loc" style="color: var(--danger);">Error</div>
              <div class="sym-popup-actions">
                ${renderPopupWindowControlsHtml(inst)}
              </div>
            </div>
            <div style="padding: 10px; color: var(--danger); font-size: 11px;">HTTP error ${resp.status}</div>
          `;
          syncPopupShadeUI(inst);
        }
        if (!inst.moved && !inst.resizedW && !inst.resizedH) {
          positionSymbolPopup(inst, btnEl);
        }
        return true;
      }

      const data = await resp.json();
      if (signal.aborted) return true;
      if (!data || !data.found) {
        closeSpecificSymbolPopup(inst);
        return false;
      }

      data.in_repo = true;
      data.def_path = data.path || peerPath;

      const stateCtx = {
        repo: repo,
        change: change,
        leftRev: data.left_rev || '',
        rightRev: data.right_rev || '',
        base: base,
        target: target,
        side: peerSide,
        mode: getCurrentPageDiffMode(),
        originPath: peerPath,
      };

      inst.currentState = {
        kind: 'move',
        data: data,
        ctx: stateCtx,
        scrollTop: 0,
      };

      renderMovePopup(inst, data, stateCtx, undefined, false);
      if (!inst.moved && !inst.resizedW && !inst.resizedH) {
        positionSymbolPopup(inst, btnEl);
      }
      return true;
    } catch (err) {
      if (signal.aborted) return true;
      closeSpecificSymbolPopup(inst);
      return false;
    }
  }
  window.handleOpenMovePeerPopup = handleOpenMovePeerPopup;

  document.addEventListener('click', async (e) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey || e.detail !== 1) {
      return;
    }
    const sel = window.getSelection ? window.getSelection() : null;
    if (sel && !sel.isCollapsed) {
      return;
    }
    if (e.target.closest && e.target.closest('.thread-card, .comment-form, a, button, .symbol-popup, #symbolPopup')) {
      return;
    }
    const tok = e.target.closest ? e.target.closest('.tok-id, .tok-typ, .tok-imp, .tok-doc') : null;
    if (!tok) {
      return;
    }
    const codeTd = tok.closest ? tok.closest('td[data-side]') : null;
    if (!codeTd) {
      return;
    }
    const container = codeTd.closest ? codeTd.closest('.diff-container') : null;
    if (!container) {
      return;
    }
    const tr = codeTd.closest ? codeTd.closest('tr') : null;
    if (!tr) {
      return;
    }

    const side = (codeTd.getAttribute && codeTd.getAttribute('data-side')) || ((tr.getAttribute && tr.getAttribute('data-line-r')) ? 'R' : 'L');
    const lineAttr = side === 'L' ? 'data-line-l' : 'data-line-r';
    const line = parseInt((tr.getAttribute && tr.getAttribute(lineAttr)) || '0', 10);
    if (line <= 0) return;
    const col = getCharOffsetInElement(codeTd, tok, 0);

    await openSymbolPopupAt(container, side, line, col, tok, true);
  });

  function getDiffChunks() {
    const chunkRows = Array.from(document.querySelectorAll('.diff-table tr.row-add, .diff-table tr.row-del, .diff-table tr.row-change, .diff-table tr.row-change-add, .diff-table td.cell-add, .diff-table td.cell-del'));
    const chunks = [];
    let lastRow = null;
    for (const cellOrRow of chunkRows) {
      const tr = cellOrRow.tagName === 'TR' ? cellOrRow : (cellOrRow.closest ? cellOrRow.closest('tr') : null);
      if (!tr || tr.offsetParent === null || tr === lastRow) continue;
      if (!lastRow || tr.previousElementSibling !== lastRow) {
        chunks.push(tr);
      }
      lastRow = tr;
    }
    return chunks;
  }

  function getThreadCards() {
    return Array.from(document.querySelectorAll('.thread-card:not(.new-comment-card)')).filter(c => c.offsetParent !== null);
  }

  function isNavItemInViewport(el, minY, viewH) {
    if (!el || el.isConnected === false || !el.getBoundingClientRect) return false;
    const r = el.getBoundingClientRect();
    if (typeof r.width === 'number' && typeof r.height === 'number' && r.width === 0 && r.height === 0) {
      return false;
    }
    const top = typeof r.top === 'number' ? r.top : 0;
    const bottom = typeof r.bottom === 'number' ? r.bottom : (top + (el.offsetHeight || 40));
    return bottom > minY && top < viewH;
  }

  function stepViewportNav(items, direction, state) {
    if (!items || items.length === 0) return null;
    const headerEl = document.querySelector('.app-header');
    const headerBottom = (headerEl && headerEl.getBoundingClientRect) ? (headerEl.getBoundingClientRect().bottom || 0) : 0;
    const headerH = Math.max(40, headerBottom || 50);
    const viewH = window.innerHeight || (document.documentElement && document.documentElement.clientHeight) || 800;
    const minY = Math.min(headerH, viewH * 0.4);

    const curIdx = state.el ? items.indexOf(state.el) : -1;
    const curScrollY = window.scrollY || 0;
    const hasActive = curIdx >= 0 && (
      (state.scrollY >= 0 && Math.abs(curScrollY - state.scrollY) <= 4) ||
      isNavItemInViewport(state.el, minY, viewH)
    );

    let target = null;
    if (hasActive) {
      target = items[(curIdx + direction + items.length) % items.length];
    } else if (direction > 0) {
      target = items.find(el => el.getBoundingClientRect().top > headerH + 4) || items[0];
    } else {
      for (let i = items.length - 1; i >= 0; i--) {
        if (items[i].getBoundingClientRect().top < headerH - 4) {
          target = items[i];
          break;
        }
      }
      if (!target) target = items[items.length - 1];
    }

    target.scrollIntoView({ block: 'center' });
    state.el = target;
    state.scrollY = window.scrollY || 0;

    const targetRow = (target && target.tagName === 'TR') ? target : (target && target.closest ? target.closest('tr') : null);
    if (targetRow) {
      const isDiffRow = (targetRow.hasAttribute && (targetRow.hasAttribute('data-line-r') || targetRow.hasAttribute('data-line-l'))) ||
        (targetRow.getAttribute && (targetRow.getAttribute('data-line-r') !== null || targetRow.getAttribute('data-line-l') !== null)) ||
        (targetRow.classList && targetRow.classList.contains && targetRow.classList.contains('diff-row')) ||
        (typeof targetRow.className === 'string' && (targetRow.className.includes('diff-row') || targetRow.className.includes('row-')));
      if (isDiffRow) {
        if (activeCursorRow && activeCursorRow !== targetRow) {
          activeCursorRow.style.outline = '';
        }
        activeCursorRow = targetRow;
        pendingCursorRow = targetRow;
      }
    }
    return target;
  }
  window.getDiffChunks = getDiffChunks;
  window.getThreadCards = getThreadCards;
  window.chunkNavState = chunkNavState;
  window.threadNavState = threadNavState;

  // Keyboard navigation
  let cursorRowIndex = 0;
  let cachedVisibleRows = null;
  let activeCursorRow = null;
  let pendingCursorRow = null;

  function invalidateVisibleRowsCache() {
    cachedVisibleRows = null;
    if (activeCursorRow && activeCursorRow.offsetParent === null) {
      activeCursorRow.style.outline = '';
      activeCursorRow = null;
    }
  }

  function rows() {
    if (!cachedVisibleRows) {
      cachedVisibleRows = Array.from(document.querySelectorAll('.diff-table tr[data-line-r], .diff-table tr[data-line-l]')).filter(r => r.offsetParent !== null);
    }
    if (pendingCursorRow) {
      const rIdx = cachedVisibleRows.indexOf(pendingCursorRow);
      if (rIdx >= 0) {
        cursorRowIndex = rIdx;
      }
      pendingCursorRow = null;
    }
    return cachedVisibleRows;
  }

  function isGoDiffSymbolClick(targetEl, e) {
    if (!targetEl || typeof targetEl.closest !== 'function' || !e) return false;
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey || e.detail !== 1) {
      return false;
    }
    const sel = window.getSelection ? window.getSelection() : null;
    if (sel && !sel.isCollapsed) {
      return false;
    }
    if (targetEl.closest('.thread-card, .comment-form, a, button, .symbol-popup, #symbolPopup')) {
      return false;
    }
    const tok = targetEl.closest('.tok-id, .tok-typ, .tok-imp, .tok-doc');
    if (!tok || typeof tok.closest !== 'function') return false;
    const codeTd = tok.closest('td[data-side]');
    if (!codeTd || typeof codeTd.closest !== 'function') return false;
    const container = codeTd.closest('.diff-container');
    if (!container) return false;
    const tr = codeTd.closest('tr');
    if (!tr) return false;

    const side = (codeTd.getAttribute && codeTd.getAttribute('data-side')) || ((tr.getAttribute && tr.getAttribute('data-line-r')) ? 'R' : 'L');
    const lineAttr = side === 'L' ? 'data-line-l' : 'data-line-r';
    const line = parseInt((tr.getAttribute && tr.getAttribute(lineAttr)) || '0', 10);
    if (line <= 0) return false;

    const filePath = (container.getAttribute && container.getAttribute('data-file-path')) || '';
    const extPkg = (container.getAttribute && container.getAttribute('data-ext-pkg')) || '';
    const extFile = (container.getAttribute && container.getAttribute('data-ext-file')) || '';
    return (extPkg && extFile) ? extFile.endsWith('.go') : filePath.endsWith('.go');
  }

  document.addEventListener('click', (e) => {
    if (!e || !e.target) return;
    const targetEl = e.target.nodeType === 3 ? e.target.parentElement : e.target;
    if (!targetEl || typeof targetEl.closest !== 'function') return;

    if (e.button === undefined || e.button === 0) {
      const popupEl = targetEl.closest('.symbol-popup') || targetEl.closest('#symbolPopup');
      const inst = popupEl ? openSymbolPopups.find(p => p && p.el === popupEl) : null;
      let selectingInsidePopup = false;
      if (!inst && openSymbolPopups.length > 0 && window.getSelection) {
        const sel = window.getSelection();
        if (sel && !sel.isCollapsed && sel.anchorNode) {
          const anchorEl = sel.anchorNode.nodeType === 3 ? sel.anchorNode.parentElement : sel.anchorNode;
          const anchorPopupEl = anchorEl && anchorEl.closest ? (anchorEl.closest('.symbol-popup') || anchorEl.closest('#symbolPopup')) : null;
          if (anchorPopupEl && openSymbolPopups.some(p => p && p.el === anchorPopupEl)) {
            selectingInsidePopup = true;
          }
        }
      }
      if (!selectingInsidePopup && (inst || !isGoDiffSymbolClick(targetEl, e))) {
        dismissUnpinnedPopupsExcept(inst || null);
      }
    }

    const tr = targetEl.closest('.diff-table tr[data-line-r], .diff-table tr[data-line-l], tr.diff-row');
    if (!tr) return;
    if (activeCursorRow && activeCursorRow !== tr) {
      activeCursorRow.style.outline = '';
    }
    activeCursorRow = tr;
    pendingCursorRow = tr;
  }, true);

  function navigateFiles(direction) {
    const sections = Array.from(document.querySelectorAll('.diff-file-section'));
    if (sections.length > 0) {
      const viewportTop = window.scrollY + 60;
      let curIdx = 0;
      for (let i = 0; i < sections.length; i++) {
        const top = sections[i].offsetTop;
        if (top <= viewportTop) {
          curIdx = i;
        } else {
          break;
        }
      }
      let targetIdx = curIdx + direction;
      if (targetIdx < 0) targetIdx = 0;
      if (targetIdx >= sections.length) targetIdx = sections.length - 1;
      sections[targetIdx].scrollIntoView({ behavior: 'smooth', block: 'start' });
      return;
    }

    if (direction > 0) {
      const nextFile = document.getElementById('nextFileLink');
      if (nextFile) nextFile.click();
    } else {
      const prevFile = document.getElementById('prevFileLink');
      if (prevFile) prevFile.click();
    }
  }

  let bracketHandledOnKeydown = false;

  function submitFormElement(form) {
    if (!form) return false;
    if (typeof form.requestSubmit === 'function') {
      form.requestSubmit();
      return true;
    }
    const submitEvt = (typeof Event === 'function')
      ? new Event('submit', { bubbles: true, cancelable: true })
      : { type: 'submit', bubbles: true, cancelable: true, defaultPrevented: false, target: form, preventDefault() { this.defaultPrevented = true; } };
    if (typeof form.dispatchEvent === 'function') {
      const notCancelled = form.dispatchEvent(submitEvt);
      if (!submitEvt.defaultPrevented && notCancelled !== false && typeof form.onsubmit === 'function') {
        if (!submitEvt.target) {
          form.onsubmit({ preventDefault() {}, target: form });
        } else {
          form.onsubmit(submitEvt);
        }
      }
      return true;
    }
    if (typeof form.onsubmit === 'function') {
      form.onsubmit(submitEvt);
      return true;
    }
    return false;
  }

  function handleKeyAction(e) {
    if (e.defaultPrevented) return;

    if (e.key === 'Escape') {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const activeEl = document.activeElement || e.target;
      const handled = triggerEscapeAction(activeEl, { shiftKey: !!e.shiftKey });
      if (handled) {
        e.preventDefault();
      }
      return;
    }

    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      const target = e.target || document.activeElement;
      const commentFormWrap = (target && target.closest) ? target.closest('.comment-form, .msg-edit-form') : null;
      if (commentFormWrap) {
        if (e.preventDefault) e.preventDefault();
        const form = (target.closest && target.closest('form')) || (commentFormWrap.querySelector && commentFormWrap.querySelector('form'));
        if (submitFormElement(form)) {
          return;
        }
        if (commentFormWrap.classList && commentFormWrap.classList.contains('msg-edit-form') && commentFormWrap.id && commentFormWrap.id.startsWith('msg-edit-')) {
          const rawId = commentFormWrap.id.slice('msg-edit-'.length);
          const idNum = Number(rawId);
          window.submitEditMessage(Number.isFinite(idNum) && rawId !== '' ? idNum : rawId);
          return;
        }
        if (commentFormWrap.classList && commentFormWrap.classList.contains('reply-form') && commentFormWrap.id && commentFormWrap.id.startsWith('reply-form-')) {
          const rawId = commentFormWrap.id.slice('reply-form-'.length);
          const idNum = Number(rawId);
          window.replyThread({ preventDefault() {}, target: commentFormWrap }, Number.isFinite(idNum) && rawId !== '' ? idNum : rawId);
          return;
        }
        if (commentFormWrap.classList && commentFormWrap.classList.contains('new-comment-card')) {
          return;
        }
      }
      const targetForm = (target && target.closest) ? target.closest('form') : null;
      if (targetForm) {
        if (e.preventDefault) e.preventDefault();
        submitFormElement(targetForm);
      }
      return;
    }

    const target = e.target || document.activeElement;
    const tag = target ? target.tagName : '';
    const isInput = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || (target && target.isContentEditable);
    if (isInput) {
      return;
    }

    // MANDATORY: Ignore modifier keys (Cmd+C / Ctrl+C / Alt / etc.) so copy never triggers 'c'!
    if (e.metaKey || e.ctrlKey || e.altKey) {
      return;
    }

    if (e.key === '?' || (e.shiftKey && e.key === '/')) {
      e.preventDefault();
      toggleShortcutsModal();
      return;
    }
    if (e.key === '/') {
      const search = document.getElementById('changeSearchInput');
      if (search) {
        e.preventDefault();
        search.focus();
        search.select();
      }
      return;
    }

    if (e.key === 'j') {
      const diffRows = rows();
      if (diffRows.length > 0) {
        cursorRowIndex = Math.min(cursorRowIndex + 1, diffRows.length - 1);
        const nextRow = diffRows[cursorRowIndex];
        if (activeCursorRow && activeCursorRow !== nextRow) {
          activeCursorRow.style.outline = '';
        }
        activeCursorRow = nextRow;
        activeCursorRow.style.outline = '2px solid var(--link)';
        activeCursorRow.scrollIntoView({ block: 'nearest' });
      }
      return;
    }
    if (e.key === 'k') {
      const diffRows = rows();
      if (diffRows.length > 0) {
        cursorRowIndex = Math.max(cursorRowIndex - 1, 0);
        const nextRow = diffRows[cursorRowIndex];
        if (activeCursorRow && activeCursorRow !== nextRow) {
          activeCursorRow.style.outline = '';
        }
        activeCursorRow = nextRow;
        activeCursorRow.style.outline = '2px solid var(--link)';
        activeCursorRow.scrollIntoView({ block: 'nearest' });
      }
      return;
    }

    if (e.key === 'n') {
      stepViewportNav(getDiffChunks(), 1, chunkNavState);
      return;
    }
    if (e.key === 'p') {
      stepViewportNav(getDiffChunks(), -1, chunkNavState);
      return;
    }

    if (e.key === 'N') {
      const target = stepViewportNav(getThreadCards(), 1, threadNavState);
      if (target) {
        bringCommentToFront(target);
        flashThreadCard(target);
      }
      return;
    }
    if (e.key === 'P') {
      const target = stepViewportNav(getThreadCards(), -1, threadNavState);
      if (target) {
        bringCommentToFront(target);
        flashThreadCard(target);
      }
      return;
    }

    if (e.key === 'e' || e.key === 'E') {
      if (document.querySelector && document.querySelector('.diff-container, .diff-all-files, .thread-card')) {
        if (e.preventDefault) e.preventDefault();
        setAllCommentsCollapsed(e.key === 'E');
      }
      return;
    }

    if (e.key === 'M') {
      markReviewedAndNext();
      return;
    }
    if (e.key === 'r') {
      window.location.reload();
      return;
    }

    // 'c': comment on current line or selection
    if (e.key === 'c') {
      e.preventDefault();
      const sel = window.getSelection();
      if (sel && !sel.isCollapsed) {
        captureDiffSelection();
      }
      let targetTr = activeRow;
      if (!targetTr) {
        const diffRows = rows();
        if (diffRows.length > 0) {
          targetTr = diffRows[cursorRowIndex];
        }
      }
      if (targetTr) {
        const line = (selectedEnd > 0 && selectedContainer === targetTr.closest('.diff-container')) ? selectedEnd : parseInt(targetTr.getAttribute('data-line-r') || targetTr.getAttribute('data-line-l') || '0', 10);
        const side = (selectedSide && selectedContainer === targetTr.closest('.diff-container')) ? selectedSide : (targetTr.getAttribute('data-line-r') ? 'R' : 'L');
        openCommentForm(targetTr, line, side, true);
      }
      return;
    }

    if (e.key === 'u') {
      const upLink = document.getElementById('upLink');
      if (upLink) upLink.click();
      return;
    }

    if (e.key === ']') {
      e.preventDefault();
      bracketHandledOnKeydown = true;
      navigateFiles(1);
      return;
    }
    if (e.key === '[') {
      e.preventDefault();
      bracketHandledOnKeydown = true;
      navigateFiles(-1);
      return;
    }

    if (e.key === 'G') {
      const snapBtn = document.getElementById('snapshotBtn');
      if (snapBtn) snapBtn.click();
      return;
    }
    if (e.key === 'a') {
      const pubBtn = document.getElementById('publishBtn');
      if (pubBtn) pubBtn.click();
      return;
    }
    if (e.key === 'm') {
      const revBtn = document.getElementById('toggleReviewedBtn');
      if (revBtn) {
        revBtn.click();
        return;
      }
      let targetSection = null;
      if (activeCursorRow && activeCursorRow.offsetParent !== null) {
        targetSection = activeCursorRow.closest('.diff-file-section');
      }
      if (!targetSection) {
        const sections = document.querySelectorAll('.diff-file-section');
        const headerOffset = 80;
        for (const sec of sections) {
          const rect = sec.getBoundingClientRect();
          if (rect.bottom > headerOffset && rect.top < window.innerHeight) {
            targetSection = sec;
            break;
          }
        }
      }
      if (targetSection) {
        const btn = targetSection.querySelector('button[data-reviewed-btn]');
        if (btn) {
          btn.click();
          return;
        }
      }
      return;
    }
  }

  // Window capture phase for keyboard shortcuts
  window.addEventListener('keydown', handleKeyAction, true);

  // Keyup fallback for brackets in case an extension intercepted keydown
  window.addEventListener('keyup', (e) => {
    const tag = e.target.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    if (e.key === ']' || e.key === '[') {
      if (!bracketHandledOnKeydown) {
        navigateFiles(e.key === ']' ? 1 : -1);
      }
      bracketHandledOnKeydown = false;
    }
  }, true);

  // Treat right-click on blank areas as Escape (dismissing popups/forms/selection)
  function isPointOverHoveredText(target, clientX, clientY) {
    if (!target || target === document || target === document.body || target === document.documentElement) {
      return false;
    }

    // Check inline text tags
    if (target.closest && (
        target.closest('span[class*="tok-"], [class*="tok-"], code, mark, strong, em, b, i, u, s') ||
        target.closest('.tok-kw, .tok-str, .tok-num, .tok-comment, .tok-id, .tok-typ, .tok-type, .tok-builtin, .tok-op, .tok-imp, .tok-doc')
    )) {
      return true;
    }

    // Non-empty line number cell
    if (target.closest) {
      const lineNumEl = target.closest('.line-num');
      if (lineNumEl && !lineNumEl.classList.contains('thread-gutter') &&
          !lineNumEl.classList.contains('skip-gutter') &&
          !lineNumEl.classList.contains('cell-empty')) {
        if ((lineNumEl.textContent || '').trim() !== '') {
          return true;
        }
      }
    }

    // Check direct text node bounding rects under target, trimming whitespace
    if (typeof document.createRange === 'function') {
      try {
        const range = document.createRange();
        const children = target.childNodes || [];
        for (let i = 0; i < children.length; i++) {
          const node = children[i];
          if (node && node.nodeType === 3 /* TEXT_NODE */) {
            const val = node.nodeValue || '';
            const leadingM = val.match(/^\s*/);
            const trailingM = val.match(/\s*$/);
            const s = leadingM ? leadingM[0].length : 0;
            const eOff = val.length - (trailingM ? trailingM[0].length : 0);
            if (s < eOff) {
              range.setStart(node, s);
              range.setEnd(node, eOff);
              const rects = range.getClientRects ? range.getClientRects() : [];
              for (let j = 0; j < rects.length; j++) {
                const r = rects[j];
                if (r && r.width > 0 && r.height > 0 &&
                    clientX >= r.left && clientX <= r.right &&
                    clientY >= r.top && clientY <= r.bottom) {
                  return true;
                }
              }
            }
          }
        }
      } catch (_) {}
    }

    return false;
  }

  document.addEventListener('contextmenu', function(e) {
    if (e.defaultPrevented) return;

    // Keyboard context menu (e.g. Menu key or Shift+F10 on Linux/Windows)
    if (e.button === 0 && e.clientX === 0 && e.clientY === 0) return;

    // Text selection active
    const sel = window.getSelection ? window.getSelection() : null;
    if (sel && !sel.isCollapsed && sel.toString().length > 0) return;

    // Interactive or form elements
    const target = e.target;
    if (!target) return;
    if (target.closest && target.closest('input, textarea, select, [contenteditable="true"], a[href], button, summary, label, img, video, audio')) {
      return;
    }

    const popupEl = target.closest ? target.closest('.symbol-popup') : null;
    if (popupEl) {
      const inst = openSymbolPopups.find(p => p && p.el === popupEl);
      if (inst) {
        if (isPointOverHoveredText(target, e.clientX, e.clientY)) {
          return;
        }
        activePopupInst = inst;
        bringPopupToFront(inst);
        e.preventDefault();
        if (e.shiftKey) {
          closeSpecificSymbolPopup(inst);
          return;
        }
        if (!stepPopupBackOrCancel(inst)) {
          closeSpecificSymbolPopup(inst);
        }
        return;
      }
    }

    // Over rendered text or syntax tokens
    if (isPointOverHoveredText(target, e.clientX, e.clientY)) {
      return;
    }

    if (e.shiftKey) {
      activePopupInst = null;
      if (closeAllSymbolPopups()) {
        e.preventDefault();
      }
      return;
    }

    // Blank area outside popups: trigger Escape action with forceClosePopups: true
    activePopupInst = null;
    const handled = triggerEscapeAction(document.activeElement || document.body, { forceClosePopups: true });
    if (handled) {
      e.preventDefault();
    }
  });

  // --- Instant Identifier Occurrence Highlighting ---
  const occHighlightedSpans = [];
  const occCreatedMarks = [];
  let currentOccIdent = '';

  function clearIdentOccurrences() {
    currentOccIdent = '';
    for (let i = 0; i < occHighlightedSpans.length; i++) {
      occHighlightedSpans[i].classList.remove('ident-occ-hl');
      if (occHighlightedSpans[i].removeAttribute) {
        occHighlightedSpans[i].removeAttribute('data-occ-ident');
      }
    }
    occHighlightedSpans.length = 0;

    if (occCreatedMarks.length > 0) {
      const parents = new Set();
      for (let i = 0; i < occCreatedMarks.length; i++) {
        const mark = occCreatedMarks[i];
        const p = mark.parentNode;
        if (p) {
          parents.add(p);
          const txt = document.createTextNode(mark.textContent);
          p.replaceChild(txt, mark);
        }
      }
      occCreatedMarks.length = 0;
      const sel = (typeof window !== 'undefined' && window.getSelection) ? window.getSelection() : null;
      const selRange = (sel && !sel.isCollapsed && sel.rangeCount > 0) ? sel.getRangeAt(0) : null;
      parents.forEach(p => {
        if (p && p.normalize) {
          if (selRange) {
            try {
              if (selRange.intersectsNode && selRange.intersectsNode(p)) return;
              if (p.contains && (p.contains(selRange.startContainer) || p.contains(selRange.endContainer))) return;
            } catch (_) {}
          }
          p.normalize();
        }
      });
    }
  }

  function isWordChar(ch) {
    return !!ch && /[A-Za-z0-9_$]/.test(ch);
  }

  function getCharBeforeElement(container, el) {
    if (!container || !el) return '';
    try {
      const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT);
      walker.currentNode = el;
      let prev = walker.previousNode();
      while (prev) {
        if (prev.textContent.length > 0) {
          return prev.textContent.charAt(prev.textContent.length - 1);
        }
        prev = walker.previousNode();
      }
    } catch (_) {}
    return '';
  }

  function getCharAfterElement(container, el) {
    let cur = el;
    while (cur && cur !== container) {
      if (cur.nextSibling) {
        cur = cur.nextSibling;
        if (cur.nodeType === Node.TEXT_NODE && cur.textContent.length > 0) {
          return cur.textContent.charAt(0);
        }
        if (cur.nodeType === Node.ELEMENT_NODE) {
          try {
            const walker = document.createTreeWalker(cur, NodeFilter.SHOW_TEXT);
            const first = walker.nextNode();
            if (first && first.textContent.length > 0) {
              return first.textContent.charAt(0);
            }
          } catch (_) {}
        }
      } else {
        cur = cur.parentElement;
      }
    }
    return '';
  }

  function rangeOverlapsMatch(selRange, textNode, start, end) {
    if (!selRange) return false;
    if (selRange.intersectsNode && !selRange.intersectsNode(textNode)) {
      return false;
    }
    if (selRange.startContainer === textNode && selRange.endContainer === textNode) {
      return Math.max(start, selRange.startOffset) < Math.min(end, selRange.endOffset);
    }
    if (selRange.startContainer === textNode) {
      return end > selRange.startOffset;
    }
    if (selRange.endContainer === textNode) {
      return start < selRange.endOffset;
    }
    return true;
  }

  function getAdjacentWordRunInfo(codeContainer, tn, offset) {
    if (!codeContainer || !tn || tn.nodeType !== 3) return null;
    const curText = tn.textContent || '';
    if (offset < 0) offset = 0;
    if (offset > curText.length) offset = curText.length;

    let s = offset;
    let e = offset;
    while (s > 0 && isWordChar(curText.charAt(s - 1))) s--;
    while (e < curText.length && isWordChar(curText.charAt(e))) e++;
    if (s === e) {
      if (offset > 0 && isWordChar(curText.charAt(offset - 1))) {
        s = offset - 1;
        while (s > 0 && isWordChar(curText.charAt(s - 1))) s--;
        e = offset;
      } else if (offset < curText.length && isWordChar(curText.charAt(offset))) {
        s = offset;
        e = offset + 1;
        while (e < curText.length && isWordChar(curText.charAt(e))) e++;
      } else {
        return null;
      }
    }

    let startNode = tn;
    let startOffset = s;
    let endNode = tn;
    let endOffset = e;

    let prefix = curText.slice(s, offset);
    let suffix = curText.slice(offset, e);
    let beforeChar = '';
    if (s === 0) {
      try {
        const walker = document.createTreeWalker(codeContainer, NodeFilter.SHOW_TEXT);
        walker.currentNode = tn;
        let prev = walker.previousNode();
        while (prev) {
          const pt = prev.textContent || '';
          let ps = pt.length;
          while (ps > 0 && isWordChar(pt.charAt(ps - 1))) ps--;
          if (ps < pt.length) {
            prefix = pt.slice(ps) + prefix;
            startNode = prev;
            startOffset = ps;
          }
          if (ps > 0) {
            beforeChar = pt.charAt(ps - 1);
            break;
          }
          prev = walker.previousNode();
        }
      } catch (_) {}
    } else {
      beforeChar = curText.charAt(s - 1);
    }

    let afterChar = '';
    if (e === curText.length) {
      try {
        const walker = document.createTreeWalker(codeContainer, NodeFilter.SHOW_TEXT);
        walker.currentNode = tn;
        let next = walker.nextNode();
        while (next) {
          const nt = next.textContent || '';
          let ne = 0;
          while (ne < nt.length && isWordChar(nt.charAt(ne))) ne++;
          if (ne > 0) {
            suffix = suffix + nt.slice(0, ne);
            endNode = next;
            endOffset = ne;
          }
          if (ne < nt.length) {
            afterChar = nt.charAt(ne);
            break;
          }
          next = walker.nextNode();
        }
      } catch (_) {}
    } else {
      afterChar = curText.charAt(e);
    }

    const fullWord = prefix + suffix;
    if (!fullWord || fullWord.length > 128 || !/^[A-Za-z_$][A-Za-z0-9_$]*$/.test(fullWord)) {
      return null;
    }
    if (isWordChar(beforeChar) || isWordChar(afterChar)) {
      return null;
    }
    return {
      word: fullWord,
      startNode,
      startOffset,
      endNode,
      endOffset,
    };
  }

  function getAdjacentWordRun(codeContainer, tn, offset) {
    const info = getAdjacentWordRunInfo(codeContainer, tn, offset);
    return info ? info.word : null;
  }
  window.getAdjacentWordRun = getAdjacentWordRun;

  function wordRunContainsPoint(info, clientX, clientY) {
    if (!info) return false;
    if (!document.createRange) return true;
    try {
      const rng = document.createRange();
      rng.setStart(info.startNode, info.startOffset);
      rng.setEnd(info.endNode, info.endOffset);
      const rects = rng.getClientRects ? rng.getClientRects() : null;
      let matchedRect = false;
      let hasRects = false;
      if (rects && rects.length > 0) {
        for (let i = 0; i < rects.length; i++) {
          const rr = rects[i];
          if (rr && rr.width > 0 && rr.height > 0) {
            hasRects = true;
            if (clientX >= rr.left - 2 && clientX <= rr.right + 2 &&
                clientY >= rr.top - 2 && clientY <= rr.bottom + 2) {
              matchedRect = true;
              break;
            }
          }
        }
      }
      if (!matchedRect) {
        const br = rng.getBoundingClientRect ? rng.getBoundingClientRect() : null;
        if (br && br.width > 0 && br.height > 0) {
          hasRects = true;
          if (clientX >= br.left - 2 && clientX <= br.right + 2 &&
              clientY >= br.top - 2 && clientY <= br.bottom + 2) {
            matchedRect = true;
          }
        }
      }
      return matchedRect || !hasRects;
    } catch (_) {
      return true;
    }
  }

  const IDENT_HOVER_DELAY_MS = 120;
  if (typeof window !== 'undefined') {
    window.IDENT_HOVER_DELAY_MS = IDENT_HOVER_DELAY_MS;
  }

  function applyIdentOccurrences(ident) {
    if (!ident) {
      clearIdentOccurrences();
      return;
    }
    clearIdentOccurrences();
    currentOccIdent = ident;

    let matchCount = 0;
    const maxMatches = 1000;

    const containers = [];

    const fileSections = document.querySelectorAll('.diff-file-section');
    for (let i = 0; i < fileSections.length; i++) {
      const sec = fileSections[i];
      if (sec.classList.contains('collapsed') || sec.style.display === 'none') continue;
      const cells = sec.querySelectorAll('td.line-code, td.line-code-unified');
      for (let j = 0; j < cells.length; j++) {
        containers.push(cells[j]);
      }
    }

    if (fileSections.length === 0) {
      const diffContainers = document.querySelectorAll('.diff-container');
      if (diffContainers.length > 0) {
        for (let i = 0; i < diffContainers.length; i++) {
          const dc = diffContainers[i];
          if (dc.style.display === 'none') continue;
          const cells = dc.querySelectorAll('td.line-code, td.line-code-unified');
          for (let j = 0; j < cells.length; j++) {
            containers.push(cells[j]);
          }
        }
      } else {
        const cells = document.querySelectorAll('td.line-code, td.line-code-unified');
        for (let j = 0; j < cells.length; j++) {
          if (cells[j].style.display !== 'none') containers.push(cells[j]);
        }
      }
    }

    const commitBox = document.querySelector('pre.commit-msg-box');
    if (commitBox && commitBox.style.display !== 'none') {
      containers.push(commitBox);
    }

    for (let i = 0; i < openSymbolPopups.length; i++) {
      const p = openSymbolPopups[i];
      if (!p || !p.el || p.shaded || p.el.style.display === 'none') continue;
      const popCodeAreas = p.el.querySelectorAll('.sym-popup-sig, .sym-popup-snippet td.sym-snippet-code, .sym-ref-code');
      if (popCodeAreas.length > 0) {
        for (let j = 0; j < popCodeAreas.length; j++) {
          containers.push(popCodeAreas[j]);
        }
      } else {
        const fallbackAreas = p.el.querySelectorAll('.sym-popup-sig, .sym-popup-snippet, .sym-ref-code');
        for (let j = 0; j < fallbackAreas.length; j++) {
          containers.push(fallbackAreas[j]);
        }
      }
    }

    const sel = (typeof window !== 'undefined' && window.getSelection) ? window.getSelection() : null;
    const selRange = (sel && !sel.isCollapsed && sel.rangeCount > 0) ? sel.getRangeAt(0) : null;

    for (let cIdx = 0; cIdx < containers.length; cIdx++) {
      if (matchCount >= maxMatches) break;
      const c = containers[cIdx];
      const cText = c.textContent;
      if (!cText || cText.indexOf(ident) === -1) continue;

      // Fast path: syntax spans
      const spans = c.querySelectorAll('.tok-id, .tok-typ, .tok-type, .tok-builtin');
      for (let sIdx = 0; sIdx < spans.length; sIdx++) {
        if (matchCount >= maxMatches) break;
        const sp = spans[sIdx];
        if (sp.textContent === ident) {
          const before = getCharBeforeElement(c, sp);
          const after = getCharAfterElement(c, sp);
          if (!isWordChar(before) && !isWordChar(after)) {
            sp.classList.add('ident-occ-hl');
            if (sp.setAttribute) {
              sp.setAttribute('data-occ-ident', ident);
            }
            occHighlightedSpans.push(sp);
            matchCount++;
          }
        }
      }

      // General path: text nodes (including across mark.intra)
      let walker = null;
      try {
        walker = document.createTreeWalker(c, NodeFilter.SHOW_TEXT);
      } catch (_) {}
      if (!walker) continue;

      const textNodes = [];
      const nodeStarts = [];
      const nodeEnds = [];
      let offset = 0;
      let cur = walker.nextNode();
      while (cur) {
        textNodes.push(cur);
        nodeStarts.push(offset);
        offset += cur.textContent.length;
        nodeEnds.push(offset);
        cur = walker.nextNode();
      }

      // Find whole-word matches in cText
      const cellMatches = [];
      let pos = 0;
      while ((pos = cText.indexOf(ident, pos)) !== -1) {
        const start = pos;
        const end = pos + ident.length;
        pos = end;

        const before = start === 0 ? '' : cText.charAt(start - 1);
        const after = end === cText.length ? '' : cText.charAt(end);
        if (isWordChar(before) || isWordChar(after)) {
          continue;
        }
        cellMatches.push({ start, end });
      }

      // Process matches in reverse order so splitText does not invalidate earlier offsets
      let nodeCursor = textNodes.length - 1;
      for (let mIdx = cellMatches.length - 1; mIdx >= 0; mIdx--) {
        if (matchCount >= maxMatches) break;
        const m = cellMatches[mIdx];

        // Find intersecting text nodes in O(M + N) using descending cursor
        while (nodeCursor >= 0 && nodeStarts[nodeCursor] >= m.end) {
          nodeCursor--;
        }
        const intersectingIndices = [];
        for (let i = nodeCursor; i >= 0 && nodeEnds[i] > m.start; i--) {
          if (nodeEnds[i] > nodeStarts[i]) {
            intersectingIndices.push(i);
          }
        }
        if (intersectingIndices.length === 0) continue;
        intersectingIndices.reverse();

        // If any intersecting node is already inside .ident-occ-hl, skip
        let alreadyHighlighted = false;
        for (let i = 0; i < intersectingIndices.length; i++) {
          const tn = textNodes[intersectingIndices[i]];
          if (tn.parentElement && tn.parentElement.closest('.ident-occ-hl')) {
            alreadyHighlighted = true;
            break;
          }
        }
        if (alreadyHighlighted) continue;

        // If selection overlaps, skip
        if (selRange) {
          let selOverlaps = false;
          for (let i = 0; i < intersectingIndices.length; i++) {
            const idx = intersectingIndices[i];
            const tn = textNodes[idx];
            const fs = Math.max(m.start, nodeStarts[idx]) - nodeStarts[idx];
            const fe = Math.min(m.end, nodeEnds[idx]) - nodeStarts[idx];
            if (rangeOverlapsMatch(selRange, tn, fs, fe)) {
              selOverlaps = true;
              break;
            }
          }
          if (selOverlaps) continue;
        }

        const isMultiFrag = intersectingIndices.length > 1;
        // Process intersecting nodes from last to first
        for (let i = intersectingIndices.length - 1; i >= 0; i--) {
          const idx = intersectingIndices[i];
          const tn = textNodes[idx];
          const fs = Math.max(m.start, nodeStarts[idx]) - nodeStarts[idx];
          const fe = Math.min(m.end, nodeEnds[idx]) - nodeStarts[idx];

          let markClass = 'ident-occ-mark';
          if (isMultiFrag) {
            if (i === 0) {
              markClass = 'ident-occ-mark ident-occ-frag-start';
            } else if (i === intersectingIndices.length - 1) {
              markClass = 'ident-occ-mark ident-occ-frag-end';
            } else {
              markClass = 'ident-occ-mark ident-occ-frag-mid';
            }
          }

          let targetNode = tn;
          if (fe < targetNode.textContent.length) {
            targetNode.splitText(fe);
          }
          if (fs > 0) {
            targetNode = targetNode.splitText(fs);
          }
          if (!targetNode.parentNode || !targetNode.textContent) continue;

          const mark = document.createElement('mark');
          mark.className = markClass;
          mark.setAttribute('data-occ-ident', ident);
          mark.textContent = targetNode.textContent;
          targetNode.parentNode.replaceChild(mark, targetNode);
          occCreatedMarks.push(mark);
        }
        matchCount++;
      }
    }
  }

  function resolveHoveredIdent(target, clientX, clientY) {
    if (!target || typeof target.closest !== 'function') return null;
    if (target.closest('input, textarea, .comment-form, .reply-form, .msg-edit-form, button, .sym-popup-header')) {
      return null;
    }

    const codeContainer = target.closest('td.line-code, td.line-code-unified, pre.commit-msg-box, .sym-popup-sig, .sym-popup-snippet, .sym-ref-code');
    if (!codeContainer) return null;

    // Do not highlight or pin occurrences inside strings, import paths, or non-doc comments
    if (target.closest('.tok-str, .tok-imp') ||
        (target.closest('.tok-com, .tok-comment') && !target.closest('.tok-doc'))) {
      return null;
    }

    // Zero-flicker stability guard
    const occEl = target.closest('.ident-occ-hl, mark.ident-occ-mark');
    if (occEl) {
      if (occEl.getBoundingClientRect) {
        const r = occEl.getBoundingClientRect();
        if (r && r.width > 0 && r.height > 0) {
          if (clientX < r.left - 2 || clientX > r.right + 2 || clientY < r.top - 2 || clientY > r.bottom + 2) {
            return null;
          }
        }
      }
      const dataOcc = occEl.getAttribute ? occEl.getAttribute('data-occ-ident') : null;
      if (currentOccIdent && (dataOcc === currentOccIdent || occEl.textContent === currentOccIdent)) {
        return currentOccIdent;
      }
    }

    // Token span fast path
    const tokEl = target.closest('.tok-id, .tok-typ, .tok-type, .tok-builtin');
    if (tokEl) {
      if (tokEl.getBoundingClientRect) {
        const r = tokEl.getBoundingClientRect();
        if (r && r.width > 0 && r.height > 0) {
          if (clientX < r.left - 2 || clientX > r.right + 2 || clientY < r.top - 2 || clientY > r.bottom + 2) {
            return null;
          }
        }
      }
      const raw = tokEl.textContent || '';
      if (/^[A-Za-z_$][A-Za-z0-9_$]*$/.test(raw) && raw.length >= 1 && raw.length <= 128) {
        const before = getCharBeforeElement(codeContainer, tokEl);
        const after = getCharAfterElement(codeContainer, tokEl);
        if (!isWordChar(before) && !isWordChar(after)) {
          return raw;
        }
      }
      return null;
    }

    // Skip non-identifier syntax tokens
    if (target.closest('.tok-kw, .tok-str, .tok-com, .tok-comment, .tok-num, .tok-op')) {
      return null;
    }

    let targetNode = null;
    let targetOffset = 0;

    if (document.caretPositionFromPoint) {
      try {
        const pos = document.caretPositionFromPoint(clientX, clientY);
        if (pos && pos.offsetNode) {
          targetNode = pos.offsetNode;
          targetOffset = pos.offset;
        }
      } catch (_) {}
    } else if (document.caretRangeFromPoint) {
      try {
        const r = document.caretRangeFromPoint(clientX, clientY);
        if (r && r.startContainer) {
          targetNode = r.startContainer;
          targetOffset = r.startOffset;
        }
      } catch (_) {}
    }

    if (!targetNode && target) {
      if (target.nodeType === 3) {
        targetNode = target;
        targetOffset = 0;
      } else if (target.childNodes) {
        for (let i = 0; i < target.childNodes.length; i++) {
          if (target.childNodes[i].nodeType === 3) {
            targetNode = target.childNodes[i];
            targetOffset = 0;
            break;
          }
        }
      }
    }

    if (targetNode && targetNode.nodeType === 3) {
      const info = getAdjacentWordRunInfo(codeContainer, targetNode, targetOffset);
      if (info && wordRunContainsPoint(info, clientX, clientY)) return info.word;
    }

    const textNodes = [];
    if (target.nodeType === 3) {
      textNodes.push(target);
    } else if (target.childNodes) {
      for (let i = 0; i < target.childNodes.length; i++) {
        const cn = target.childNodes[i];
        if (cn.nodeType === 3 && cn.textContent) {
          textNodes.push(cn);
        }
      }
    }

    for (let i = 0; i < textNodes.length; i++) {
      const tn = textNodes[i];
      const text = tn.textContent;
      const wordRe = /[A-Za-z_$][A-Za-z0-9_$]*/g;
      let m;
      while ((m = wordRe.exec(text)) !== null) {
        const info = getAdjacentWordRunInfo(codeContainer, tn, m.index);
        if (info && wordRunContainsPoint(info, clientX, clientY)) return info.word;
      }
    }

    return null;
  }

  function updateIdentOccurrencesAt(target, clientX, clientY) {
    if (identHoverTimer) {
      clearTimeout(identHoverTimer);
      identHoverTimer = null;
    }
    const ident = resolveHoveredIdent(target, clientX, clientY);
    if (!ident) {
      clearIdentOccurrences();
      return;
    }
    if (ident === currentOccIdent && (occHighlightedSpans.length > 0 || occCreatedMarks.length > 0)) {
      return;
    }
    applyIdentOccurrences(ident);
  }

  function refreshIdentOccurrences() {
    if (currentOccIdent) {
      applyIdentOccurrences(currentOccIdent);
    }
  }
  const updateIdentOccurrences = refreshIdentOccurrences;

  let identHoverTimer = null;
  let lastHoverTarget = null;
  let lastHoverX = 0;
  let lastHoverY = 0;

  function handlePointerMove(e) {
    if (e.buttons !== undefined && e.buttons !== 0) {
      return;
    }
    const target = e.target;
    const x = e.clientX !== undefined ? e.clientX : 0;
    const y = e.clientY !== undefined ? e.clientY : 0;

    const codeContainer = (target && target.closest) ? target.closest('td.line-code, td.line-code-unified, pre.commit-msg-box, .sym-popup-sig, .sym-popup-snippet, .sym-ref-code') : null;
    if (!codeContainer) {
      if (identHoverTimer) {
        clearTimeout(identHoverTimer);
        identHoverTimer = null;
      }
      if (currentOccIdent) {
        clearIdentOccurrences();
      }
      lastHoverTarget = null;
      return;
    }

    const inNonIdentToken = target.closest ?
      (target.closest('.tok-str, .tok-imp') ||
       (target.closest('.tok-com, .tok-comment') && !target.closest('.tok-doc'))) : null;
    const occEl = (!inNonIdentToken && target.closest) ? target.closest('.ident-occ-hl, mark.ident-occ-mark') : null;
    const dataOcc = occEl && occEl.getAttribute ? occEl.getAttribute('data-occ-ident') : null;
    if (occEl && currentOccIdent && (dataOcc === currentOccIdent || occEl.textContent === currentOccIdent)) {
      if (occEl.getBoundingClientRect) {
        const r = occEl.getBoundingClientRect();
        if (!r || (r.width > 0 && r.height > 0 && x >= r.left - 2 && x <= r.right + 2 && y >= r.top - 2 && y <= r.bottom + 2)) {
          if (identHoverTimer) {
            clearTimeout(identHoverTimer);
            identHoverTimer = null;
          }
          return;
        }
      } else {
        if (identHoverTimer) {
          clearTimeout(identHoverTimer);
          identHoverTimer = null;
        }
        return;
      }
    }

    lastHoverTarget = target;
    lastHoverX = x;
    lastHoverY = y;

    if (identHoverTimer) {
      clearTimeout(identHoverTimer);
    }
    identHoverTimer = setTimeout(() => {
      identHoverTimer = null;
      updateIdentOccurrencesAt(lastHoverTarget, lastHoverX, lastHoverY);
    }, IDENT_HOVER_DELAY_MS);
  }

  if (typeof document !== 'undefined' && document.addEventListener) {
    document.addEventListener('pointermove', handlePointerMove, { passive: true });
    if (typeof window === 'undefined' || !window.PointerEvent) {
      document.addEventListener('mousemove', handlePointerMove, { passive: true });
      document.addEventListener('mouseover', handlePointerMove, { passive: true });
    }
    document.addEventListener('mouseleave', () => {
      if (identHoverTimer) {
        clearTimeout(identHoverTimer);
        identHoverTimer = null;
      }
      clearIdentOccurrences();
    });
    if (typeof window !== 'undefined' && window.addEventListener) {
      window.addEventListener('blur', () => {
        if (identHoverTimer) {
          clearTimeout(identHoverTimer);
          identHoverTimer = null;
        }
        clearIdentOccurrences();
      });
    }
  }

  function restoreSavedDrafts(targetContainer) {
    pruneOldDrafts();
    const root = targetContainer || document;
    const info = getCurrentRepoAndChange(root);
    if (!info.repo || !info.change) return;

    for (const [k, d] of formDraftsMap.entries()) {
      const text = d && (d.content || d.body);
      const hasText = !!(text && String(text).trim());
      if (!d || (!hasText && !(d.message_id && d.unresolved !== undefined))) continue;
      if (d.thread_id) {
        const form = root.querySelector('#reply-form-' + d.thread_id);
        if (form && (form.style.display === 'none' || form.style.display === '')) {
          window.toggleReplyForm(d.thread_id, false, { noFocus: true });
        }
      } else if (d.message_id) {
        const form = root.querySelector('#msg-edit-' + d.message_id);
        if (form && (form.style.display === 'none' || form.style.display === '')) {
          window.openEditMessageForm(d.message_id, { noFocus: true });
        }
      } else if (d.target_path && d.line > 0) {
        if (document.getElementById('inlineNewComment')) continue;
        const cleanPath = d.target_path.replace(/^\/+/, '');
        const containers = Array.from(root.querySelectorAll('.diff-container'));
        let targetCont = null;
        for (const c of containers) {
          const cp = (c.getAttribute('data-file-path') || '').replace(/^\/+/, '');
          if (cp === cleanPath) {
            targetCont = c;
            break;
          }
        }
        if (!targetCont) continue;
        const side = d.side || 'R';
        const attr = side === 'L' ? 'data-line-l' : 'data-line-r';
        const startRow = targetCont.querySelector(`[${attr}="${d.line}"]`) || targetCont.querySelector(`tr[${attr}="${d.line}"]`);
        if (!startRow) continue;

        selectedStart = d.line;
        selectedEnd = d.end_line || d.line;
        selectedStartCol = d.start_col || 0;
        selectedEndCol = d.end_col || 0;
        selectedSide = side;
        selectedContainer = targetCont;

        const endRow = (selectedEnd !== selectedStart)
          ? (targetCont.querySelector(`[${attr}="${selectedEnd}"]`) || targetCont.querySelector(`tr[${attr}="${selectedEnd}"]`) || startRow)
          : startRow;
        activeRow = endRow;
        window.openCommentForm(endRow, selectedEnd, side, true, { noFocus: true });
        if (selectedStart < selectedEnd || selectedStartCol > 0 || selectedEndCol > 0) {
          if (typeof highlightSelectedRange === 'function') {
            highlightSelectedRange();
          }
        }
      }
    }
  }

  const THREADS_MENU_TRIGGER_SELECTOR = '#changeCommentCounts .threads-menu-trigger, #changeCommentCounts .unresolved-threads-trigger, #changeCommentCounts .resolved-threads-trigger, #changeCommentCounts .draft-threads-trigger';

  function enhanceChangeUnresolvedBadge() {
    const container = document.getElementById('changeCommentCounts');
    if (!container || !container.querySelector) return;

    const enhanceBadge = (badge, triggerClass, status, re, suffix) => {
      if (!badge || (badge.classList && badge.classList.contains('threads-menu-trigger'))) return;
      if (badge.classList) {
        badge.classList.add(triggerClass, 'threads-menu-trigger');
      }
      if (badge.setAttribute) {
        badge.setAttribute('role', 'button');
        badge.setAttribute('tabindex', '0');
        badge.setAttribute('aria-haspopup', 'true');
        badge.setAttribute('aria-expanded', 'false');
        badge.setAttribute('data-status', status);
      }
      const text = badge.textContent || '';
      const m = text.match(re);
      if (m) {
        badge.textContent = `${m[1]} ${suffix} ▾`;
      }
    };

    const unresBadge = container.querySelector('.badge-count-unresolved');
    enhanceBadge(unresBadge, 'unresolved-threads-trigger', 'unresolved', /(\d+)\s+unresolved/, 'unresolved');

    const resBadge = container.querySelector('.badge-count-resolved');
    enhanceBadge(resBadge, 'resolved-threads-trigger', 'resolved', /(\d+)\s+resolved/, 'resolved');

    const draftBadge = container.querySelector('.badge-draft');
    enhanceBadge(draftBadge, 'draft-threads-trigger', 'draft', /(\d+)\s+draft/, 'draft');

    const menu = document.querySelector ? document.querySelector('.unresolved-threads-menu') : null;
    if (menu && menu.style && menu.style.display !== 'none') {
      const activeStatus = menu.getAttribute ? menu.getAttribute('data-active-status') : '';
      if (activeStatus === 'unresolved' || activeStatus === 'resolved' || activeStatus === 'draft') {
        const activeBadge = activeStatus === 'resolved'
          ? resBadge
          : (activeStatus === 'draft' ? draftBadge : unresBadge);
        if (activeBadge) {
          if (activeBadge.setAttribute) {
            activeBadge.setAttribute('aria-expanded', 'true');
          }
          positionThreadsMenu(menu, activeBadge);
        } else {
          closeUnresolvedMenu();
        }
      }
    }
  }

  async function jumpToUnresolvedThread(threadId, cleanPath, messageId, snapshotNum, inCurrentView) {
    closeUnresolvedMenu();
    if (!threadId) return;

    const countsEl = document.getElementById('changeCommentCounts');
    const info = getCurrentRepoAndChange(countsEl);
    const base = (countsEl && countsEl.getAttribute('data-base')) || '';
    const target = (countsEl && countsEl.getAttribute('data-target')) || '';
    const SearchParamsCtor = (typeof URLSearchParams === 'function')
      ? URLSearchParams
      : (window.URLSearchParams || null);
    const sp = SearchParamsCtor ? new SearchParamsCtor(window.location.search || '') : null;
    const mode = sp ? (sp.get('mode') || '') : '';
    const ctxParam = sp ? (sp.get('ctx') || '') : '';
    const msgId = messageId ? String(messageId) : '';
    const targetHash = msgId ? `#thread-${threadId}-msg-${msgId}` : `#thread-${threadId}`;
    const normalizedPath = String(cleanPath || '').replace(/^\/+/, '');
    const hasDiffsAll = !!document.querySelector('.diff-all-files') || !!document.querySelector('.diff-file-section');

    if (inCurrentView === false || inCurrentView === 'false') {
      if (!info.repo || !info.change) return;
      const destTarget = (snapshotNum && snapshotNum > 0) ? ('snap:' + snapshotNum) : 'live';
      const destBase = 'parent';
      const pathname = (window.location && window.location.pathname) ? String(window.location.pathname) : '';
      const onAllDiffsPage = hasDiffsAll && !pathname.includes('/f/');
      if (onAllDiffsPage) {
        let dest = `/r/${encodeURIComponent(info.repo)}/c/${encodeURIComponent(info.change)}?diffs=all&base=${encodeURIComponent(destBase)}&target=${encodeURIComponent(destTarget)}`;
        if (mode) dest += `&mode=${encodeURIComponent(mode)}`;
        if (ctxParam) dest += `&ctx=${encodeURIComponent(ctxParam)}`;
        window.location.href = dest + targetHash;
        return;
      }
      if (normalizedPath) {
        const encodedPath = normalizedPath.split('/').map(encodeURIComponent).join('/');
        let dest = `/r/${encodeURIComponent(info.repo)}/c/${encodeURIComponent(info.change)}/f/${encodedPath}?base=${encodeURIComponent(destBase)}&target=${encodeURIComponent(destTarget)}`;
        if (mode) dest += `&mode=${encodeURIComponent(mode)}`;
        if (ctxParam) dest += `&ctx=${encodeURIComponent(ctxParam)}`;
        window.location.href = dest + targetHash;
      }
      return;
    }

    const buildFileDest = () => {
      if (!info.repo || !info.change || !normalizedPath) return '';
      const encodedPath = normalizedPath.split('/').map(encodeURIComponent).join('/');
      let dest = `/r/${encodeURIComponent(info.repo)}/c/${encodeURIComponent(info.change)}/f/${encodedPath}?base=${encodeURIComponent(base)}&target=${encodeURIComponent(target)}`;
      if (mode) dest += `&mode=${encodeURIComponent(mode)}`;
      if (ctxParam) dest += `&ctx=${encodeURIComponent(ctxParam)}`;
      return dest + targetHash;
    };

    if (!hasDiffsAll) {
      const dest = buildFileDest();
      if (dest) window.location.href = dest;
      return;
    }

    let sec = null;
    const allSecs = document.querySelectorAll('.diff-file-section');
    for (const s of allSecs) {
      const p = (s.getAttribute('data-file-path') || '').replace(/^\/+/, '');
      if (p === normalizedPath) {
        sec = s;
        break;
      }
    }

    let expandedSec = false;
    if (sec) {
      if (sec.classList.contains('collapsed')) {
        const body = sec.querySelector('.diff-file-body');
        const btn = sec.querySelector('.file-collapse-btn');
        sec.classList.remove('collapsed');
        if (body) body.style.display = 'block';
        if (btn) btn.textContent = '▾';
        expandedSec = true;
        if (window.ensureFileBodyLoaded && body) {
          await window.ensureFileBodyLoaded(body);
        }
      } else {
        const body = sec.querySelector('.diff-file-body');
        if (body && body.getAttribute('data-lazy-url') && !body.hasAttribute('data-loaded')) {
          if (window.ensureFileBodyLoaded) {
            await window.ensureFileBodyLoaded(body);
          }
        }
      }
    }
    if (expandedSec) {
      if (typeof handleNaturalPageHeightChange === 'function') handleNaturalPageHeightChange();
      if (typeof window.updateToggleAllFilesBtn === 'function') window.updateToggleAllFilesBtn();
      saveExpandedFiles();
      if (typeof invalidateVisibleRowsCache === 'function') invalidateVisibleRowsCache();
      scheduleStickyScopeUpdate();
    }

    const card = document.getElementById('thread-' + threadId);
    const pathname = (window.location && window.location.pathname) ? String(window.location.pathname) : '';
    if (!card && normalizedPath && !pathname.includes('/f/')) {
      const dest = buildFileDest();
      if (dest) window.location.href = dest;
      return;
    }

    if (card) {
      revealThreadForJump(card, msgId);
      const candidateMsgEl = msgId ? document.getElementById('msg-' + msgId) : null;
      const msgEl = (candidateMsgEl && (!card.contains || card.contains(candidateMsgEl))) ? candidateMsgEl : null;
      const scrollTarget = msgEl || card;
      if (scrollTarget && scrollTarget.scrollIntoView) {
        scrollTarget.scrollIntoView({ block: 'center' });
      }
      bringCommentToFront(card);
      if (!card.classList.contains('new-comment-card')) {
        threadNavState.el = card;
        threadNavState.scrollY = window.scrollY || 0;
      }
      flashThreadCard(card);
      try {
        if (typeof history !== 'undefined' && history.replaceState) {
          history.replaceState(null, '', targetHash);
        } else {
          location.hash = targetHash;
        }
      } catch (_) {}
    }
  }

  let threadsMenuReqSeq = 0;
  let threadsMenuAbort = null;

  function positionThreadsMenu(menu, triggerEl) {
    if (!menu || !triggerEl || !triggerEl.getBoundingClientRect) return;
    const vw = (document.documentElement && document.documentElement.clientWidth) || window.innerWidth || 1000;
    const vh = (document.documentElement && document.documentElement.clientHeight) || window.innerHeight || 800;
    const scrollX = window.scrollX || 0;
    const scrollY = window.scrollY || 0;
    const rect = triggerEl.getBoundingClientRect();
    const margin = 12;
    const gap = 4;

    menu.style.left = (rect.left + scrollX) + 'px';
    menu.style.top = (rect.bottom + scrollY + gap) + 'px';

    const mRect = menu.getBoundingClientRect ? menu.getBoundingClientRect() : null;
    if (mRect && mRect.width > 0) {
      const maxLeft = scrollX + vw - mRect.width - margin;
      const clampedLeft = Math.max(scrollX + margin, Math.min(rect.left + scrollX, maxLeft));
      menu.style.left = clampedLeft + 'px';
    }

    const spaceBelow = vh - rect.bottom - margin;
    const spaceAbove = rect.top - margin;
    if (spaceBelow < 180 && spaceAbove > spaceBelow) {
      const maxH = Math.max(100, Math.min(480, Math.floor(spaceAbove - gap)));
      menu.style.maxHeight = maxH + 'px';
      const curRect = menu.getBoundingClientRect ? menu.getBoundingClientRect() : mRect;
      const actualH = Math.min(maxH, (curRect && curRect.height) || maxH);
      menu.style.top = Math.max(scrollY + margin, rect.top + scrollY - actualH - gap) + 'px';
    } else {
      const maxH = Math.max(100, Math.min(480, Math.floor(spaceBelow - gap)));
      menu.style.maxHeight = maxH + 'px';
      menu.style.top = (rect.bottom + scrollY + gap) + 'px';
    }
  }

  function toggleUnresolvedMenu(triggerEl) {
    if (!triggerEl) return;
    let menu = document.querySelector('.unresolved-threads-menu');
    const isExpanded = triggerEl.getAttribute('aria-expanded') === 'true';

    if (isExpanded) {
      closeUnresolvedMenu();
      return;
    }

    const seq = ++threadsMenuReqSeq;
    if (threadsMenuAbort) {
      try { threadsMenuAbort.abort(); } catch (_) {}
      threadsMenuAbort = null;
    }
    threadsMenuAbort = (typeof AbortController === 'function') ? new AbortController() : null;

    // Reset other triggers
    document.querySelectorAll(THREADS_MENU_TRIGGER_SELECTOR).forEach(t => {
      if (t !== triggerEl) t.setAttribute('aria-expanded', 'false');
    });

    if (!menu) {
      menu = document.createElement('div');
      menu.className = 'unresolved-threads-menu';
      document.body.appendChild(menu);

      menu.addEventListener('click', (e) => {
        const row = e.target.closest ? e.target.closest('.unres-item-row') : null;
        if (row) {
          const threadId = row.getAttribute('data-thread-id');
          const cleanPath = row.getAttribute('data-path');
          const messageId = row.getAttribute('data-message-id') || '';
          const snapAttr = row.getAttribute('data-snapshot-num');
          const snapshotNum = snapAttr ? parseInt(snapAttr, 10) : 0;
          const inViewAttr = row.getAttribute('data-in-current-view');
          const inCurrentView = (inViewAttr === null || inViewAttr === '') ? undefined : (inViewAttr !== 'false');
          jumpToUnresolvedThread(threadId, cleanPath, messageId, snapshotNum, inCurrentView);
        }
      });

      menu.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          const row = e.target.closest ? e.target.closest('.unres-item-row') : null;
          if (row) {
            e.preventDefault();
            const threadId = row.getAttribute('data-thread-id');
            const cleanPath = row.getAttribute('data-path');
            const messageId = row.getAttribute('data-message-id') || '';
            const snapAttr = row.getAttribute('data-snapshot-num');
            const snapshotNum = snapAttr ? parseInt(snapAttr, 10) : 0;
            const inViewAttr = row.getAttribute('data-in-current-view');
            const inCurrentView = (inViewAttr === null || inViewAttr === '') ? undefined : (inViewAttr !== 'false');
            jumpToUnresolvedThread(threadId, cleanPath, messageId, snapshotNum, inCurrentView);
          }
        }
      });
    }

    const countsEl = triggerEl.closest('#changeCommentCounts') || document.getElementById('changeCommentCounts');
    const info = getCurrentRepoAndChange(countsEl);
    const base = (countsEl && countsEl.getAttribute('data-base')) || '';
    const target = (countsEl && countsEl.getAttribute('data-target')) || '';

    const status = triggerEl.getAttribute('data-status') || (
      triggerEl.classList.contains('resolved-threads-trigger')
        ? 'resolved'
        : (triggerEl.classList.contains('draft-threads-trigger') ? 'draft' : 'unresolved')
    );
    const listNoun = status === 'draft'
      ? 'draft comments'
      : (status === 'resolved' ? 'resolved threads' : 'unresolved threads');

    menu.style.display = 'block';
    if (menu.setAttribute) {
      menu.setAttribute('data-active-status', status);
    }
    triggerEl.setAttribute('aria-expanded', 'true');

    menu.innerHTML = `<div class="unres-item-empty">Loading ${listNoun}…</div>`;
    positionThreadsMenu(menu, triggerEl);

    const resolveLiveTrigger = () => {
      if (triggerEl && triggerEl.isConnected !== false) {
        return triggerEl;
      }
      const live = document.querySelector ? document.querySelector(`#changeCommentCounts .threads-menu-trigger[data-status="${status}"]`) : null;
      return live || null;
    };

    const url = `/api/threads/list?status=${encodeURIComponent(status)}&repo=${encodeURIComponent(info.repo)}&change=${encodeURIComponent(info.change)}&base=${encodeURIComponent(base)}&target=${encodeURIComponent(target)}`;
    const fetchOpts = threadsMenuAbort ? { signal: threadsMenuAbort.signal } : undefined;
    fetch(url, fetchOpts)
      .then(res => {
        if (seq !== threadsMenuReqSeq) return null;
        if (!res.ok) throw new Error('failed');
        return res.json();
      })
      .then(data => {
        if (seq !== threadsMenuReqSeq || !menu || menu.style.display === 'none' || !data) {
          return;
        }
        const liveTrigger = resolveLiveTrigger();
        if (!liveTrigger) {
          closeUnresolvedMenu();
          return;
        }
        if (!data.threads || data.threads.length === 0) {
          menu.innerHTML = `<div class="unres-item-empty">No ${listNoun}</div>`;
          positionThreadsMenu(menu, liveTrigger);
          return;
        }
        let html = '';
        for (const th of data.threads) {
          let loc = th.path;
          if (th.line > 0) {
            loc += `:L${th.line}`;
            if (th.end_line && th.end_line > th.line) {
              loc += `–${th.end_line}`;
            }
          }
          let badges = '';
          if (th.snapshot_num > 0) {
            badges += `<span class="badge badge-xs" style="margin-left: 6px;">Snapshot ${escapeHtml(th.snapshot_num)}</span>`;
          }
          if (th.has_draft && status !== 'draft') {
            badges += '<span class="badge badge-draft badge-xs" style="margin-left: 6px;">draft</span>';
          }
          if (th.stale) {
            badges += '<span class="badge badge-xs" style="margin-left: 6px; background: var(--danger); color: #fff;">stale</span>';
          }
          const authorHtml = th.author ? `<span class="unres-item-author">${escapeHtml(th.author)}</span>` : '';
          const previewHtml = th.preview_html
            ? th.preview_html
            : `<p class="md-p">${escapeHtml(th.preview || 'No preview')}</p>`;
          const isResolved = (typeof th.resolved === 'boolean') ? th.resolved : (status === 'resolved');
          const statusClass = isResolved ? 'item-resolved' : 'item-unresolved';
          const msgAttr = th.message_id ? ` data-message-id="${escapeHtml(th.message_id)}"` : '';
          const snapAttr = (th.snapshot_num > 0) ? ` data-snapshot-num="${escapeHtml(th.snapshot_num)}"` : '';
          const inViewAttr = (typeof th.in_current_view === 'boolean') ? ` data-in-current-view="${th.in_current_view ? 'true' : 'false'}"` : '';
          html += `<div class="unres-item-row ${statusClass}" role="button" tabindex="0" data-thread-id="${escapeHtml(th.id)}"${msgAttr}${snapAttr}${inViewAttr} data-path="${escapeHtml(th.path)}">` +
            `<div class="unres-item-loc-line"><span class="unres-item-loc-left"><span class="unres-item-loc">${escapeHtml(loc)}</span>${badges}</span>${authorHtml}</div>` +
            `<div class="unres-item-preview markdown-body">${previewHtml}</div>` +
          `</div>`;
        }
        menu.innerHTML = html;
        positionThreadsMenu(menu, liveTrigger);
      })
      .catch(err => {
        if (err && err.name === 'AbortError') return;
        if (seq !== threadsMenuReqSeq || !menu || menu.style.display === 'none') return;
        const liveTrigger = resolveLiveTrigger();
        if (!liveTrigger) {
          closeUnresolvedMenu();
          return;
        }
        menu.innerHTML = '<div class="unres-item-empty" style="color: var(--danger, #cf222e);">Failed to load threads</div>';
        positionThreadsMenu(menu, liveTrigger);
      });
  }

  function closeUnresolvedMenu() {
    threadsMenuReqSeq++;
    if (threadsMenuAbort) {
      try { threadsMenuAbort.abort(); } catch (_) {}
      threadsMenuAbort = null;
    }
    const menu = document.querySelector('.unresolved-threads-menu');
    if (menu) {
      menu.style.display = 'none';
      if (menu.removeAttribute) {
        menu.removeAttribute('data-active-status');
      }
    }
    const triggers = document.querySelectorAll(THREADS_MENU_TRIGGER_SELECTOR);
    triggers.forEach(t => t.setAttribute('aria-expanded', 'false'));
  }

  document.addEventListener('click', (e) => {
    const trigger = e.target.closest ? e.target.closest(THREADS_MENU_TRIGGER_SELECTOR) : null;
    if (trigger) {
      e.preventDefault();
      e.stopPropagation();
      toggleUnresolvedMenu(trigger);
      return;
    }
    const menu = document.querySelector('.unresolved-threads-menu');
    if (menu && menu.style.display !== 'none' && !menu.contains(e.target)) {
      closeUnresolvedMenu();
    }
  });

  document.addEventListener('keydown', (e) => {
    const trigger = e.target.closest ? e.target.closest(THREADS_MENU_TRIGGER_SELECTOR) : null;
    if (trigger) {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        toggleUnresolvedMenu(trigger);
      }
    }
  });

  window.updateIdentOccurrencesAt = updateIdentOccurrencesAt;
  window.resolveHoveredIdent = resolveHoveredIdent;
  window.updateIdentOccurrences = updateIdentOccurrences;
  window.clearIdentOccurrences = clearIdentOccurrences;
  window.applyIdentOccurrences = applyIdentOccurrences;
  window.formDraftsMap = formDraftsMap;
  window.restoreSavedDrafts = restoreSavedDrafts;
  window.pruneOldDrafts = pruneOldDrafts;
  window.enhanceChangeUnresolvedBadge = enhanceChangeUnresolvedBadge;
  window.toggleUnresolvedMenu = toggleUnresolvedMenu;
  window.closeUnresolvedMenu = closeUnresolvedMenu;
  window.jumpToUnresolvedThread = jumpToUnresolvedThread;
  window.updateCommentCountDOM = updateCommentCountBadges;
})();
