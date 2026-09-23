// The recordings list, shared by every receiver page that records.
//
// Served by the audio package next to the recordings themselves, so the
// FM and airband pages cannot drift apart: both get the same list, the
// same bulk delete, and the same play-through.
//
// Recordings made in one session — Record pressed, clips gathered, Record
// released — are shown as one collapsed row that plays as a whole, and
// opens to show its clips. A recording made on its own, like an FM take,
// is a row by itself.
//
// Play-through goes forward in time within a session. Start a session, or
// any clip in it, and when one ends the one made after it plays, up to
// the end of the session — so an afternoon of airband clips can be
// listened to like the stream it was.
//
//   const recs = Recordings(document.getElementById('recordings'), {
//     empty: 'None yet.',          // shown when there are none
//     onError: msg => ...,         // where to report a failure
//   });
//   recs.load();                   // fetch the list again
//   recs.play(name);               // play one, and carry on from it
function Recordings(root, opts = {}) {
  const onError = opts.onError || (msg => console.warn(msg));
  // Encoded a segment at a time, so a session's slash stays a slash.
  const url = name => '/recordings/' + name.split('/').map(encodeURIComponent).join('/');
  const sessionOf = name => name.includes('/') ? name.split('/')[0] : '';

  if (!document.getElementById('recordings-style')) {
    const st = document.createElement('style');
    st.id = 'recordings-style';
    st.textContent = `
      .rec-bar { display:flex; gap:10px; align-items:center; margin-bottom:6px;
                 font-size:12px; color:var(--dim); }
      .rec-bar label { display:flex; gap:6px; align-items:center; cursor:pointer; }
      .rec-bar .sp { flex:1; }
      .rec-bar button { background:#21262d; color:var(--fg); border:1px solid var(--line);
                        border-radius:7px; padding:4px 10px; font:inherit; cursor:pointer; }
      .rec-bar button.danger:not(:disabled) { color:var(--bad); }
      .rec-bar button.danger:not(:disabled):hover { border-color:var(--bad); }
      .rec-bar button:disabled { opacity:.45; cursor:default; }
      .rec-list { list-style:none; margin:0; padding:0; max-height:460px; overflow-y:auto; }
      .rec-list li { display:flex; gap:10px; align-items:center; padding:5px 4px;
                     border-bottom:1px solid #21262d; font-size:12px;
                     font-variant-numeric:tabular-nums; }
      .rec-list li.group { font-size:13px; }
      .rec-list li.group .n { cursor:pointer; }
      .rec-list li.clip { padding-left:34px; background:#12161d; }
      .rec-list li.playing { background:#1c2230; box-shadow:inset 3px 0 var(--accent); }
      .rec-list .tw { width:14px; color:var(--dim); cursor:pointer; text-align:center; flex:none; }
      .rec-list .n { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
      .rec-list .n small { color:var(--dim); font-size:12px; }
      .rec-list .d { color:var(--dim); }
      .rec-list .live { color:var(--bad); font-size:11px; }
      .rec-list button, .rec-list a { background:none; border:0; color:var(--accent); font:inherit;
                                      cursor:pointer; padding:0; text-decoration:none; }
      .rec-list button.del { color:var(--dim); }
      .rec-list button.del:hover { color:var(--bad); }
      .rec-list input, .rec-bar input { accent-color:var(--accent); margin:0; }
      .rec-empty { color:var(--dim); font-size:12px; padding:10px 0; font-style:italic; }
      .rec-player { width:100%; margin-top:10px; }
      .rec-now { color:var(--dim); font-size:12px; margin-top:6px; min-height:16px; }
      .rec-total { color:var(--dim); font-size:12px; margin-top:8px; padding-top:8px;
                   border-top:1px solid var(--line); text-align:right;
                   font-variant-numeric:tabular-nums; }
    `;
    document.head.append(st);
  }

  const bar = document.createElement('div');
  bar.className = 'rec-bar';
  const allBox = document.createElement('input');
  allBox.type = 'checkbox';
  const allLbl = document.createElement('label');
  allLbl.append(allBox, 'all');
  allLbl.title = 'select every recording';
  const through = document.createElement('input');
  through.type = 'checkbox';
  through.checked = true;
  const throughLbl = document.createElement('label');
  throughLbl.append(through, 'play through');
  throughLbl.title = 'when one ends, play the one recorded after it in the same session';
  const sp = document.createElement('span');
  sp.className = 'sp';
  const delSel = document.createElement('button');
  delSel.className = 'danger';
  bar.append(allLbl, throughLbl, sp, delSel);

  const emptyEl = document.createElement('div');
  emptyEl.className = 'rec-empty';
  emptyEl.textContent = opts.empty || 'None yet.';
  const ul = document.createElement('ul');
  ul.className = 'rec-list';
  const player = document.createElement('audio');
  player.className = 'rec-player';
  player.controls = true;
  player.hidden = true;
  const now = document.createElement('div');
  now.className = 'rec-now';
  // What the recordings take up on disk, since clips left running pile up.
  const total = document.createElement('div');
  total.className = 'rec-total';
  root.replaceChildren(bar, emptyEl, ul, player, now, total);

  let list = [];               // newest first, as the server sends it
  let playing = null;          // name of the recording in the player
  const selected = new Set();  // recording names
  const open = new Set();      // sessions shown expanded

  const when = (at, style = 'medium') =>
    new Date(at).toLocaleString([], {dateStyle: 'short', timeStyle: style});
  // What a file or session was called, without the time it starts with.
  const what = s => s.replace(/^\d{8}-\d{6}_/, '').replace(/\.wav$/, '').replaceAll('_', ' ');
  const label = rec => when(rec.at) + ' · ' + what(rec.name.split('/').pop());

  const clock = s => {
    if (s < 60) return s.toFixed(1) + 's';
    s = Math.round(s);
    const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60, ss = String(s % 60).padStart(2, '0');
    return h ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
  };

  const size = b => {
    const units = ['bytes', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
    return (i === 0 ? b : b.toFixed(b < 10 ? 1 : 0)) + ' ' + units[i];
  };

  // Only finished recordings can be played, selected or deleted.
  const done = () => list.filter(r => !r.active);

  // groups turns the list into rows: one per session, one per recording
  // made on its own. Both stay newest first.
  function groups() {
    const out = [], bySession = new Map();
    for (const rec of list) {
      if (!rec.session) { out.push({clips: [rec]}); continue; }
      let g = bySession.get(rec.session);
      if (!g) { g = {session: rec.session, clips: []}; bySession.set(rec.session, g); out.push(g); }
      g.clips.push(rec);
    }
    return out;
  }

  // The recordings play-through moves between: the rest of a session, or
  // for one made on its own, the others made on their own.
  // The server leaves session out for one made on its own, hence the ''.
  const peers = name => done().filter(r => (r.session || '') === sessionOf(name));

  function syncBar() {
    // A selection can outlive the recordings in it — deleted from another
    // tab — so it is trimmed to what the list still has.
    const names = new Set(done().map(r => r.name));
    for (const n of selected) if (!names.has(n)) selected.delete(n);
    delSel.textContent = selected.size ? `Delete ${selected.size} selected` : 'Delete selected';
    delSel.disabled = selected.size === 0;
    allBox.checked = names.size > 0 && selected.size === names.size;
    allBox.indeterminate = selected.size > 0 && selected.size < names.size;
    allBox.disabled = names.size === 0;
    bar.hidden = list.length === 0;
    emptyEl.hidden = list.length > 0;
    total.hidden = list.length === 0;
    const bytes = list.reduce((t, r) => t + r.bytes, 0);
    const secs = list.reduce((t, r) => t + r.seconds, 0);
    total.textContent = `${list.length} recording${list.length === 1 ? '' : 's'} · ` +
      `${clock(secs)} · ${size(bytes)} on disk`;
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function box(names, disabled) {
    const b = el('input');
    b.type = 'checkbox';
    const n = names.filter(x => selected.has(x)).length;
    b.disabled = disabled || names.length === 0;
    b.checked = names.length > 0 && n === names.length;
    b.indeterminate = n > 0 && n < names.length;
    b.onchange = () => {
      names.forEach(x => b.checked ? selected.add(x) : selected.delete(x));
      draw();
    };
    return b;
  }

  function button(text, title, onclick, cls) {
    const b = el('button', cls, text);
    b.title = title;
    b.onclick = e => { e.stopPropagation(); onclick(); };
    return b;
  }

  // A recording's own row, whether on its own or inside an open session.
  function clipRow(rec, inGroup) {
    const li = el('li', inGroup ? 'clip' : '');
    li.classList.toggle('playing', rec.name === playing);
    li.append(box([rec.name], rec.active));
    if (!inGroup) li.append(el('span', 'tw'));
    const n = el('span', 'n', label(rec));
    n.title = rec.name;
    li.append(n, el('span', 'd', clock(rec.seconds)));
    if (rec.active) { li.append(el('span', 'live', '● recording')); return li; }
    const dl = el('a', '', '⤓');
    dl.title = 'download';
    dl.href = url(rec.name) + '?download';
    li.append(
      button('▶', inGroup ? 'play from here to the end of the session' : 'play', () => api.play(rec.name)),
      dl,
      // A long take is worth a second thought; a two-second clip is not.
      button('✕', 'delete', () => (rec.seconds < 60 || confirm('Delete ' + label(rec) + '?')) && remove([rec.name]), 'del'),
    );
    return li;
  }

  function groupRows(g) {
    const clips = g.clips;
    const finished = clips.filter(r => !r.active).map(r => r.name);
    const oldest = clips[clips.length - 1];
    const total = clips.reduce((t, r) => t + r.seconds, 0);
    const isOpen = open.has(g.session);
    const inside = clips.findIndex(r => r.name === playing);

    const li = el('li', 'group');
    li.classList.toggle('playing', inside >= 0 && !isOpen);
    const toggle = () => { isOpen ? open.delete(g.session) : open.add(g.session); draw(); };
    const tw = el('span', 'tw', isOpen ? '▾' : '▸');
    tw.onclick = toggle;
    const n = el('span', 'n');
    n.append(when(oldest.at, 'short') + ' · ' + what(g.session) + ' ',
             el('small', '', `· ${clips.length} clip${clips.length === 1 ? '' : 's'}` +
                (inside >= 0 ? ` · playing ${clips.length - inside} of ${clips.length}` : '')));
    n.title = isOpen ? 'hide the clips' : 'show the clips';
    n.onclick = toggle;
    li.append(box(finished), tw, n, el('span', 'd', clock(total)));
    if (clips.some(r => r.active)) li.append(el('span', 'live', '● recording'));
    if (finished.length) {
      li.append(
        button('▶', 'play the whole session', () => api.play(finished[finished.length - 1])),
        button('✕', 'delete the whole session', () =>
          confirm(`Delete this session and its ${finished.length} recording${finished.length === 1 ? '' : 's'}?`) &&
          remove(finished), 'del'),
      );
    }
    const rows = [li];
    if (isOpen) rows.push(...clips.map(r => clipRow(r, true)));
    return rows;
  }

  function draw() {
    const rows = [];
    for (const g of groups()) {
      if (g.session) rows.push(...groupRows(g));
      else rows.push(clipRow(g.clips[0], false));
    }
    ul.replaceChildren(...rows);
    syncBar();
  }

  async function remove(names) {
    if (names.includes(playing)) stop();
    try {
      const r = await fetch('/api/recordings/delete', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({names}),
      });
      if (!r.ok) onError((await r.text()).trim());
      else {
        const d = await r.json();
        if (d.failed && d.failed.length) onError('could not delete ' + d.failed.join(', '));
      }
    } catch (e) {
      onError('receiver offline');
    }
    names.forEach(n => selected.delete(n));
    await api.load();
  }

  function stop() {
    player.pause();
    player.removeAttribute('src');
    player.load();
    player.hidden = true;
    playing = null;
    now.textContent = '';
    draw();
  }

  // The one recorded after name in the same session: the list is newest
  // first, so that is the entry before it. The list may have changed since
  // playing began — clips arriving, others deleted — so it is looked up
  // afresh each time.
  function after(name) {
    const d = peers(name);
    const i = d.findIndex(r => r.name === name);
    if (i > 0) return d[i - 1].name;
    if (i < 0) {
      // Deleted while playing: carry on with whatever came after it in time.
      const base = name.split('/').pop();
      const next = d.filter(r => r.name.split('/').pop() > base).at(-1);
      return next ? next.name : null;
    }
    return null;
  }

  player.onended = () => {
    const next = through.checked && playing ? after(playing) : null;
    if (next) api.play(next);
    else { now.textContent = playing ? 'finished' : ''; playing = null; draw(); }
  };
  player.onerror = () => {
    if (playing) onError('could not play ' + playing);
  };

  allBox.onchange = () => {
    selected.clear();
    if (allBox.checked) done().forEach(r => selected.add(r.name));
    draw();
  };
  delSel.onclick = () => {
    const names = [...selected];
    if (names.length && confirm(`Delete ${names.length} recording${names.length > 1 ? 's' : ''}?`)) {
      remove(names);
    }
  };

  const api = {
    async load() {
      try {
        const r = await fetch('/api/recordings');
        if (!r.ok) return;
        list = await r.json();
        draw();
      } catch (e) { /* the page's own poll reports an offline receiver */ }
    },
    play(name) {
      playing = name;
      player.hidden = false;
      player.src = url(name);
      // An AbortError only means another recording was chosen before this
      // one started, which is not a failure worth reporting.
      player.play().catch(e => { if (e.name !== 'AbortError') onError('playback: ' + e.message); });
      const rec = list.find(r => r.name === name);
      const left = peers(name).findIndex(r => r.name === name);
      now.textContent = 'playing ' + (rec ? label(rec) : name) +
        (through.checked && left > 0 ? ` · ${left} more after it` : '');
      draw();
      const li = ul.querySelector('li.playing');
      if (li) li.scrollIntoView({block: 'nearest'});
    },
  };
  draw();
  return api;
}
