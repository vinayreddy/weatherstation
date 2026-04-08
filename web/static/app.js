// ---------------------------------------------------------------------------
// Dashboard: live image + current conditions + today's chart
// ---------------------------------------------------------------------------

const isDashboard = document.getElementById('live-image') !== null;
const isHistory = document.getElementById('temp-chart') !== null;

// Fields where 0 indicates missing sensor data, not a real reading.
const ZERO_IS_NULL = new Set(['temp', 'dewPoint', 'feelsLike', 'pressure']);

// Build {x, y} point array for Chart.js, converting implausible zeros to null.
// Using {x, y} objects (instead of a labels array) is required for spanGaps
// with a millisecond threshold to work on time-scale axes.
function prepareChartData(obs, field) {
    return obs.map(o => ({
        x: new Date(o.timestamp * 1000),
        y: (ZERO_IS_NULL.has(field) && o[field] === 0) ? null : o[field],
    }));
}

// Build a continuous cumulative precipitation series across daily resets.
// The WU API's precipTotal resets to 0 at midnight; this detects those drops
// and carries over the prior day's total so the line only goes up.
function cumulativePrecipData(obs) {
    let offset = 0;
    let prev = 0;
    return obs.map(o => {
        const val = o.precipTotal;
        if (val < prev) offset += prev;
        prev = val;
        return { x: new Date(o.timestamp * 1000), y: offset + val };
    });
}

if (isDashboard) {
    // Auto-refresh image every 30 seconds
    let fullscreenOpen = false;

    function refreshImage() {
        const cacheBust = '?' + Date.now();
        document.getElementById('live-image').src = '/images/current.jpg' + cacheBust;
        const timeStr = new Date().toLocaleString([], { hour: 'numeric', minute: '2-digit' });
        document.getElementById('image-time').textContent = timeStr;
        if (fullscreenOpen) {
            document.getElementById('fullscreen-image').src = '/images/current.jpg' + cacheBust;
            document.getElementById('fullscreen-time').textContent = timeStr;
        }
    }
    refreshImage();
    setInterval(refreshImage, 30000);

    // Fullscreen image overlay
    function openImageFullscreen() {
        const overlay = document.getElementById('fullscreen-overlay');
        const cacheBust = '?' + Date.now();
        document.getElementById('fullscreen-image').src = '/images/current.jpg' + cacheBust;
        document.getElementById('fullscreen-time').textContent =
            document.getElementById('image-time').textContent;
        overlay.classList.remove('hidden');
        overlay.classList.add('flex');
        fullscreenOpen = true;
    }
    window.openImageFullscreen = openImageFullscreen;

    function closeImageFullscreen() {
        const overlay = document.getElementById('fullscreen-overlay');
        overlay.classList.add('hidden');
        overlay.classList.remove('flex');
        fullscreenOpen = false;
    }
    window.closeImageFullscreen = closeImageFullscreen;

    // Convert wind degrees to 16-point cardinal direction
    function degreesToCardinal(deg) {
        const dirs = ['N','NNE','NE','ENE','E','ESE','SE','SSE',
                      'S','SSW','SW','WSW','W','WNW','NW','NNW'];
        return dirs[Math.round(deg / 22.5) % 16];
    }

    // Fetch and display current conditions
    async function updateConditions() {
        try {
            const resp = await fetch('/api/current');
            const data = await resp.json();
            const obs = data.observation;
            if (!obs) return;

            document.getElementById('temp').textContent = Math.round(obs.temp) + '\u00B0F';
            document.getElementById('feels-like').textContent = Math.round(obs.feelsLike) + '\u00B0F';
            document.getElementById('dew-point').textContent = Math.round(obs.dewPoint) + '\u00B0F';
            document.getElementById('humidity').textContent = Math.round(obs.humidity) + '%';
            document.getElementById('wind').textContent = Math.round(obs.windSpeed) + ' mph';
            document.getElementById('wind-gust').textContent = Math.round(obs.windGust) + ' mph';
            document.getElementById('pressure').textContent = obs.pressure.toFixed(2) + ' in';
            document.getElementById('precip-rate').textContent = obs.precipRate.toFixed(2) + ' in/hr';
            document.getElementById('uv').textContent = obs.uv.toFixed(1);

            // Wind compass
            const cardinal = degreesToCardinal(obs.windDir);
            document.getElementById('wind-dir-label').textContent = cardinal;
            document.getElementById('wind-arrow').setAttribute('transform', 'rotate(' + obs.windDir + ' 60 60)');

            const time = new Date(obs.timestamp * 1000);
            document.getElementById('obs-time').textContent =
                'as of ' + time.toLocaleString([], { hour: 'numeric', minute: '2-digit' });
        } catch (e) {
            console.error('Failed to fetch conditions:', e);
        }
    }
    updateConditions();
    setInterval(updateConditions, 60000);

    // Today's chart
    async function loadTodayChart() {
        const now = Math.floor(Date.now() / 1000);
        const dayStart = now - 86400;
        try {
            const resp = await fetch(`/api/observations?from=${dayStart}&to=${now}`);
            const data = await resp.json();
            const obs = data.observations || [];
            if (obs.length === 0) return;

            const ctx = document.getElementById('today-chart');
            new Chart(ctx, {
                type: 'line',
                data: {
                    datasets: [{
                        label: 'Temp (\u00B0F)',
                        data: prepareChartData(obs, 'temp'),
                        borderColor: '#ef4444',
                        backgroundColor: 'rgba(239,68,68,0.1)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 2,
                        spanGaps: 900000,
                    }, {
                        label: 'Rain (in/hr)',
                        data: prepareChartData(obs, 'precipRate'),
                        borderColor: '#3b82f6',
                        backgroundColor: 'rgba(59,130,246,0.2)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 1,
                        yAxisID: 'y1',
                        spanGaps: 900000,
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    interaction: { intersect: false, mode: 'index' },
                    plugins: {
                        legend: { labels: { color: '#9ca3af', font: { size: 11 } } },
                        tooltip: {
                            callbacks: {
                                title: (items) => items.length ? new Date(items[0].parsed.x).toLocaleString([], { hour: 'numeric', minute: '2-digit' }) : '',
                            },
                        },
                    },
                    scales: {
                        x: {
                            type: 'time',
                            time: { unit: 'hour', displayFormats: { hour: 'ha', minute: 'h:mm a' } },
                            ticks: { color: '#6b7280' },
                            grid: { color: 'rgba(75,85,99,0.3)' },
                        },
                        y: {
                            ticks: { color: '#6b7280' },
                            grid: { color: 'rgba(75,85,99,0.3)' },
                        },
                        y1: {
                            position: 'right',
                            ticks: { color: '#6b7280' },
                            grid: { display: false },
                            min: 0,
                        }
                    }
                }
            });
        } catch (e) {
            console.error('Failed to load today chart:', e);
        }
    }
    loadTodayChart();
}

// ---------------------------------------------------------------------------
// History: multi-panel charts with click-to-image
// ---------------------------------------------------------------------------

let historyCharts = [];
let currentFrom = 0, currentTo = 0;

function setActiveButton(el) {
    document.querySelectorAll('.range-btn').forEach(b => b.classList.remove('active'));
    if (el) el.classList.add('active');
}

function updateURL(params) {
    const url = new URL(window.location);
    url.search = '';
    for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
    history.replaceState(null, '', url);
}

function showLockButton(visible) {
    document.getElementById('lock-btn').classList.toggle('hidden', !visible);
}

function loadRange(days, btn) {
    setActiveButton(btn || document.querySelector(`.range-btn[data-range="${days}"]`));
    updateURL({ range: days });
    showLockButton(true);

    const now = Math.floor(Date.now() / 1000);
    const from = now - days * 86400;
    loadObservations(from, now);
}

function loadCustomRange() {
    const from = document.getElementById('date-from').value;
    const to = document.getElementById('date-to').value;
    if (!from || !to) return;
    const fromTs = Math.floor(new Date(from).getTime() / 1000);
    const toTs = Math.floor(new Date(to + 'T23:59:59').getTime() / 1000);
    setActiveButton(null);
    updateURL({ from: fromTs, to: toTs });
    showLockButton(false);
    loadObservations(fromTs, toTs);
}

function lockRange() {
    updateURL({ from: currentFrom, to: currentTo });
    setActiveButton(null);
    // Fill the date inputs to reflect the locked range
    document.getElementById('date-from').value = new Date(currentFrom * 1000).toISOString().slice(0, 10);
    document.getElementById('date-to').value = new Date(currentTo * 1000).toISOString().slice(0, 10);
    showLockButton(false);
}

function initHistoryFromURL() {
    const params = new URLSearchParams(window.location.search);
    if (params.has('from') && params.has('to')) {
        const from = parseInt(params.get('from'));
        const to = parseInt(params.get('to'));
        document.getElementById('date-from').value = new Date(from * 1000).toISOString().slice(0, 10);
        document.getElementById('date-to').value = new Date(to * 1000).toISOString().slice(0, 10);
        setActiveButton(null);
        showLockButton(false);
        loadObservations(from, to);
    } else {
        const range = parseInt(params.get('range')) || 7;
        loadRange(range);
    }
}

async function loadObservations(from, to) {
    if (!isHistory) return;
    currentFrom = from;
    currentTo = to;
    try {
        const resp = await fetch(`/api/observations?from=${from}&to=${to}`);
        const data = await resp.json();
        const obs = data.observations || [];
        renderHistoryCharts(obs);
    } catch (e) {
        console.error('Failed to load observations:', e);
    }
}

function renderHistoryCharts(obs) {
    // Destroy previous charts
    historyCharts.forEach(c => c.destroy());
    historyCharts = [];

    function fmtTime(date) {
        return date.toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
    }

    const commonOpts = {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { intersect: false, mode: 'index' },
        plugins: {
            legend: { labels: { color: '#9ca3af', font: { size: 11 } } },
            tooltip: {
                callbacks: {
                    title: (items) => items.length ? fmtTime(new Date(items[0].parsed.x)) : '',
                },
            },
        },
        scales: {
            x: {
                type: 'time',
                time: { tooltipFormat: '', displayFormats: { hour: 'ha', day: 'MMM d', minute: 'h:mm a' } },
                ticks: { color: '#6b7280', maxTicksLimit: 12 },
                grid: { color: 'rgba(75,85,99,0.3)' },
            },
            y: {
                ticks: { color: '#6b7280' },
                grid: { color: 'rgba(75,85,99,0.3)' },
            }
        },
        onClick: (e, elements) => {
            if (elements.length > 0) {
                const pt = e.chart.data.datasets[elements[0].datasetIndex].data[elements[0].index];
                if (pt && pt.x) {
                    const ts = Math.floor(pt.x.getTime() / 1000);
                    const match = obs.find(o => o.timestamp === ts);
                    if (match) showImagePopup(ts, match);
                }
            }
        }
    };

    // Temp + Dew Point
    historyCharts.push(new Chart(document.getElementById('temp-chart'), {
        type: 'line',
        data: {
            datasets: [{
                label: 'Temp (\u00B0F)', data: prepareChartData(obs, 'temp'),
                borderColor: '#ef4444', pointRadius: 0, borderWidth: 2, tension: 0.3,
                spanGaps: 900000,
            }, {
                label: 'Dew Point (\u00B0F)', data: prepareChartData(obs, 'dewPoint'),
                borderColor: '#65a30d', pointRadius: 0, borderWidth: 2, tension: 0.3,
                spanGaps: 900000,
            }]
        },
        options: commonOpts,
    }));

    // Wind
    historyCharts.push(new Chart(document.getElementById('wind-chart'), {
        type: 'line',
        data: {
            datasets: [{
                label: 'Wind (mph)', data: prepareChartData(obs, 'windSpeed'),
                borderColor: '#3b82f6', pointRadius: 0, borderWidth: 2, tension: 0.3,
                spanGaps: 900000,
            }, {
                label: 'Gusts (mph)', data: prepareChartData(obs, 'windGust'),
                borderColor: '#f97316', pointRadius: 1, pointBackgroundColor: '#f97316',
                borderWidth: 0, showLine: false,
            }]
        },
        options: commonOpts,
    }));

    // Precipitation
    historyCharts.push(new Chart(document.getElementById('precip-chart'), {
        type: 'line',
        data: {
            datasets: [{
                label: 'Precip Rate (in/hr)', data: prepareChartData(obs, 'precipRate'),
                borderColor: '#65a30d', backgroundColor: 'rgba(101,163,13,0.2)',
                fill: true, pointRadius: 0, borderWidth: 1, tension: 0.3,
                spanGaps: 900000,
            }, {
                label: 'Precip Total (in)', data: cumulativePrecipData(obs),
                borderColor: '#3b82f6', backgroundColor: 'rgba(59,130,246,0.1)',
                fill: true, pointRadius: 0, borderWidth: 2, tension: 0.3,
                yAxisID: 'y1', spanGaps: 900000,
            }]
        },
        options: {
            ...commonOpts,
            scales: {
                ...commonOpts.scales,
                y1: { position: 'right', ticks: { color: '#6b7280' }, grid: { display: false }, min: 0 },
            }
        },
    }));

    // Pressure
    historyCharts.push(new Chart(document.getElementById('pressure-chart'), {
        type: 'line',
        data: {
            datasets: [{
                label: 'Pressure (inHg)', data: prepareChartData(obs, 'pressure'),
                borderColor: '#8b5cf6', pointRadius: 0, borderWidth: 2, tension: 0.3,
                spanGaps: 900000,
            }]
        },
        options: commonOpts,
    }));
}

// ---------------------------------------------------------------------------
// Image popup (click chart point to see camera image)
// ---------------------------------------------------------------------------

async function showImagePopup(ts, obs) {
    try {
        const resp = await fetch(`/api/nearest-image?ts=${ts}`);
        if (!resp.ok) return;
        const img = await resp.json();

        document.getElementById('popup-image').src = '/images/' + img.path;
        const time = new Date(ts * 1000).toLocaleString();
        let info = time;
        if (obs) {
            info += ` | ${Math.round(obs.temp)}\u00B0F | Wind: ${Math.round(obs.windSpeed)} mph`;
        }
        document.getElementById('popup-info').textContent = info;
        document.getElementById('image-popup').classList.add('show');
        document.getElementById('image-popup').classList.remove('hidden');
    } catch (e) {
        console.error('Failed to load image:', e);
    }
}

function closePopup() {
    document.getElementById('image-popup').classList.add('hidden');
    document.getElementById('image-popup').classList.remove('show');
}

// Close popups on Escape key
document.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
        if (typeof closePopup === 'function') closePopup();
        if (typeof closeImageFullscreen === 'function') closeImageFullscreen();
    }
});
