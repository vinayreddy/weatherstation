// Highlights: the auto-scored "most interesting" frames, grouped by category.

const PT = 'America/Los_Angeles';

// Display order, labels and icons for each scoring category (see highlights.go).
const CATEGORIES = [
  { key: 'aurora',    label: 'Aurora',              icon: '🌌' },
  { key: 'snow',      label: 'Snow',                icon: '❄️' },
  { key: 'windstorm', label: 'Windstorms',          icon: '💨' },
  { key: 'extreme',   label: 'Temperature extremes', icon: '🌡️' },
  { key: 'fog',       label: 'Fog',                 icon: '🌫️' },
  { key: 'rain',      label: 'Heavy rain',          icon: '🌧️' },
  { key: 'golden',    label: 'Golden hour',         icon: '🌅' },
  { key: 'clear',     label: 'Clear days',          icon: '🏔️' },
];
const LABELS = Object.fromEntries(CATEGORIES.map(c => [c.key, c]));

let shown = []; // flat list in render order, for lightbox navigation
let lbIndex = 0;

function fmtDateTime(ts) {
  return new Date(ts * 1000).toLocaleString('en-US',
    { timeZone: PT, month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
}

// fmtWeather renders an observation as a compact one-line summary. 0 is treated
// as "missing" for temp/feels-like/pressure (matches the dashboard's ZERO_IS_NULL).
function fmtWeather(o) {
  const parts = [];
  if (o.temp) {
    let t = `${Math.round(o.temp)}°F`;
    if (o.feelsLike && Math.round(o.feelsLike) !== Math.round(o.temp)) t += ` (feels ${Math.round(o.feelsLike)}°)`;
    parts.push(t);
  }
  parts.push(`💧 ${Math.round(o.humidity)}%`);
  let wind = `💨 ${Math.round(o.windSpeed)}`;
  if (o.windGust > o.windSpeed) wind += `–${Math.round(o.windGust)}`;
  parts.push(`${wind} mph`);
  if (o.pressure) parts.push(`${o.pressure.toFixed(2)} inHg`);
  if (o.precipRate > 0) parts.push(`🌧 ${o.precipRate.toFixed(2)} in/hr`);
  return parts.join('  ·  ');
}

function makeCard(item) {
  const idx = shown.length;
  shown.push(item);
  const card = document.createElement('button');
  card.className = 'text-left bg-gray-800 rounded-lg overflow-hidden shadow-lg hover:ring-2 hover:ring-blue-500 transition';
  card.onclick = () => openLightbox(idx);

  const img = document.createElement('img');
  img.loading = 'lazy';
  img.src = `/thumb/${item.path}`;
  img.className = 'w-full aspect-[4/3] object-cover';

  const body = document.createElement('div');
  body.className = 'p-3';
  const detail = document.createElement('div');
  detail.className = 'text-sm font-medium text-gray-100';
  detail.textContent = item.detail || (LABELS[item.category]?.label ?? 'Highlight');
  const when = document.createElement('div');
  when.className = 'text-xs text-gray-400 mt-0.5';
  when.textContent = fmtDateTime(item.timestamp);
  body.appendChild(detail);
  body.appendChild(when);

  card.appendChild(img);
  card.appendChild(body);
  return card;
}

function section(title, items) {
  const wrap = document.createElement('section');
  const h = document.createElement('h3');
  h.className = 'text-lg font-semibold mb-3';
  h.textContent = title;
  const grid = document.createElement('div');
  grid.className = 'grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4';
  items.forEach(it => grid.appendChild(makeCard(it)));
  wrap.appendChild(h);
  wrap.appendChild(grid);
  return wrap;
}

async function load() {
  let highlights = [];
  try {
    const res = await fetch('/api/highlights?limit=200');
    const data = await res.json();
    highlights = data.highlights || [];
  } catch (e) { /* leave empty */ }

  document.getElementById('highlights-loading').classList.add('hidden');
  const container = document.getElementById('highlights-sections');

  if (!highlights.length) {
    document.getElementById('highlights-empty').classList.remove('hidden');
    return;
  }

  // Top picks: the highest-scoring few across every category.
  const top = highlights.slice().sort((a, b) => b.interestScore - a.interestScore).slice(0, 6);
  container.appendChild(section('⭐ Top picks', top));

  // Then a section per category (in display order), if it has any highlights.
  const byCat = {};
  highlights.forEach(h => (byCat[h.category] ||= []).push(h));
  for (const c of CATEGORIES) {
    const items = byCat[c.key];
    if (!items || !items.length) continue;
    items.sort((a, b) => b.interestScore - a.interestScore);
    container.appendChild(section(`${c.icon} ${c.label}`, items));
  }
}

// --- lightbox ----------------------------------------------------------------

function openLightbox(i) {
  lbIndex = i;
  renderLightbox();
  document.getElementById('lightbox').classList.add('show');
}
function closeLightbox() {
  document.getElementById('lightbox').classList.remove('show');
}
function renderLightbox() {
  const item = shown[lbIndex];
  if (!item) return;
  document.getElementById('lb-image').src = `/images/${item.path}`;
  const cat = LABELS[item.category];
  const tag = cat ? `${cat.icon} ${cat.label}` : '';
  document.getElementById('lb-caption').textContent =
    `${tag} · ${item.detail || ''} · ${fmtDateTime(item.timestamp)}`;
  loadLightboxWeather(item.timestamp);
}

// Show the weather nearest this frame's capture time (frames are raw images).
async function loadLightboxWeather(ts) {
  const el = document.getElementById('lb-weather');
  el.textContent = '';
  try {
    const res = await fetch(`/api/nearest-observation?ts=${ts}`);
    if (!res.ok) return;
    const o = await res.json();
    if (shown[lbIndex] && shown[lbIndex].timestamp === ts) el.textContent = fmtWeather(o);
  } catch (e) { /* leave blank */ }
}
function lbStep(n) {
  if (!shown.length) return;
  lbIndex = (lbIndex + n + shown.length) % shown.length;
  renderLightbox();
}
document.addEventListener('keydown', (e) => {
  if (!document.getElementById('lightbox').classList.contains('show')) return;
  if (e.key === 'ArrowLeft') lbStep(-1);
  else if (e.key === 'ArrowRight') lbStep(1);
  else if (e.key === 'Escape') closeLightbox();
});

document.addEventListener('DOMContentLoaded', load);
