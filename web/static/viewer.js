// Historical image browser: a thumbnail grid per day with a full-screen
// lightbox (prev/next, keyboard, and timelapse play).

const PT = 'America/Los_Angeles';
let images = [];   // current day's image records, ascending by time
let lbIndex = 0;   // lightbox cursor
let playTimer = null;

// --- date helpers (all day boundaries are Pacific, matching the server) ------

function todayPT() {
  return new Date().toLocaleDateString('en-CA', { timeZone: PT }); // YYYY-MM-DD
}

// addDays shifts a YYYY-MM-DD string by n days without tripping over DST by
// anchoring at UTC noon.
function addDays(dateStr, n) {
  const [y, m, d] = dateStr.split('-').map(Number);
  const dt = new Date(Date.UTC(y, m - 1, d, 12));
  dt.setUTCDate(dt.getUTCDate() + n);
  return dt.toISOString().slice(0, 10);
}

function fmtClock(ts) {
  return new Date(ts * 1000).toLocaleTimeString('en-US',
    { timeZone: PT, hour: 'numeric', minute: '2-digit' });
}

// --- controls ----------------------------------------------------------------

function currentDate() { return document.getElementById('viewer-date').value || todayPT(); }
function currentStep() { return document.getElementById('viewer-step').value || '0'; }

function stepDay(n) {
  document.getElementById('viewer-date').value = addDays(currentDate(), n);
  reload();
}
function goToday() {
  document.getElementById('viewer-date').value = todayPT();
  reload();
}

function syncURL() {
  const u = new URL(window.location);
  u.searchParams.set('date', currentDate());
  u.searchParams.set('step', currentStep());
  history.replaceState(null, '', u);
}

async function reload() {
  const date = currentDate();
  const step = currentStep();
  syncURL();
  // Don't let the user page into the future.
  document.getElementById('next-btn').disabled = date >= todayPT();

  const grid = document.getElementById('viewer-grid');
  const empty = document.getElementById('viewer-empty');
  grid.innerHTML = '';
  try {
    const res = await fetch(`/api/images?date=${date}&step=${step}`);
    const data = await res.json();
    images = data.images || [];
  } catch (e) {
    images = [];
  }
  empty.classList.toggle('hidden', images.length > 0);
  document.getElementById('viewer-count').textContent =
    images.length ? `${images.length} frames` : '';

  const frag = document.createDocumentFragment();
  images.forEach((img, i) => {
    const cell = document.createElement('button');
    cell.className = 'relative group rounded overflow-hidden bg-gray-800 aspect-[4/3]';
    cell.onclick = () => openLightbox(i);
    const el = document.createElement('img');
    el.loading = 'lazy';
    el.src = `/thumb/${img.path}`;
    el.className = 'w-full h-full object-cover group-hover:opacity-80 transition';
    const label = document.createElement('span');
    label.className = 'absolute bottom-0 left-0 right-0 text-[11px] bg-black/50 text-gray-200 px-1 py-0.5';
    label.textContent = fmtClock(img.timestamp);
    if (img.interestScore > 0 && img.category) {
      const star = document.createElement('span');
      star.className = 'absolute top-1 right-1 text-xs';
      star.textContent = img.pinned ? '★' : '✨'; // pinned star / highlight marker
      star.title = img.detail || img.category;
      cell.appendChild(star);
    }
    cell.appendChild(el);
    cell.appendChild(label);
    frag.appendChild(cell);
  });
  grid.appendChild(frag);
}

// --- lightbox ----------------------------------------------------------------

function openLightbox(i) {
  lbIndex = i;
  renderLightbox();
  document.getElementById('lightbox').classList.add('show');
}
function closeLightbox() {
  stopPlay();
  document.getElementById('lightbox').classList.remove('show');
}
function renderLightbox() {
  const img = images[lbIndex];
  if (!img) return;
  document.getElementById('lb-image').src = `/images/${img.path}`;
  let caption = fmtClock(img.timestamp);
  if (img.category && img.detail) caption += ` · ${img.detail}`;
  document.getElementById('lb-caption').textContent =
    `${caption}   (${lbIndex + 1}/${images.length})`;
}
function lbStep(n) {
  if (!images.length) return;
  lbIndex = (lbIndex + n + images.length) % images.length;
  renderLightbox();
}

function togglePlay() { playTimer ? stopPlay() : startPlay(); }
function startPlay() {
  playTimer = setInterval(() => lbStep(1), 600);
  document.getElementById('lb-play').innerHTML = '❚❚ Pause';
}
function stopPlay() {
  if (playTimer) clearInterval(playTimer);
  playTimer = null;
  const btn = document.getElementById('lb-play');
  if (btn) btn.innerHTML = '▶ Play';
}

document.addEventListener('keydown', (e) => {
  if (!document.getElementById('lightbox').classList.contains('show')) return;
  if (e.key === 'ArrowLeft') lbStep(-1);
  else if (e.key === 'ArrowRight') lbStep(1);
  else if (e.key === 'Escape') closeLightbox();
});

// --- init --------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  const params = new URLSearchParams(window.location.search);
  document.getElementById('viewer-date').value = params.get('date') || todayPT();
  if (params.get('step') !== null) document.getElementById('viewer-step').value = params.get('step');
  reload();
});
