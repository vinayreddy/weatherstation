// ---------------------------------------------------------------------------
// Dashboard: live image + current conditions + today's chart
// ---------------------------------------------------------------------------

const isDashboard = document.getElementById('live-image') !== null;
const isHistory = document.getElementById('temp-chart') !== null;

if (isDashboard) {
    // Auto-refresh image every 30 seconds
    setInterval(() => {
        const img = document.getElementById('live-image');
        img.src = '/images/current.jpg?' + Date.now();
    }, 30000);

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
            document.getElementById('image-time').textContent = 'Last updated: ' + time.toLocaleString();
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
                    labels: obs.map(o => new Date(o.timestamp * 1000)),
                    datasets: [{
                        label: 'Temp (\u00B0F)',
                        data: obs.map(o => o.temp),
                        borderColor: '#ef4444',
                        backgroundColor: 'rgba(239,68,68,0.1)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 2,
                    }, {
                        label: 'Rain (in/hr)',
                        data: obs.map(o => o.precipRate),
                        borderColor: '#3b82f6',
                        backgroundColor: 'rgba(59,130,246,0.2)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 1,
                        yAxisID: 'y1',
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    interaction: { intersect: false, mode: 'index' },
                    plugins: { legend: { labels: { color: '#9ca3af', font: { size: 11 } } } },
                    scales: {
                        x: {
                            type: 'time',
                            time: { unit: 'hour', displayFormats: { hour: 'ha' } },
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

function loadRange(days) {
    // Highlight active button
    document.querySelectorAll('.range-btn').forEach(b => b.classList.remove('active'));
    event.target.classList.add('active');

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
    loadObservations(fromTs, toTs);
}

async function loadObservations(from, to) {
    if (!isHistory) return;
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

    const labels = obs.map(o => new Date(o.timestamp * 1000));
    const commonOpts = {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { intersect: false, mode: 'index' },
        plugins: {
            legend: { labels: { color: '#9ca3af', font: { size: 11 } } },
        },
        scales: {
            x: {
                type: 'time',
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
                const idx = elements[0].index;
                const ts = obs[idx].timestamp;
                showImagePopup(ts, obs[idx]);
            }
        }
    };

    // Temp + Dew Point
    historyCharts.push(new Chart(document.getElementById('temp-chart'), {
        type: 'line',
        data: {
            labels,
            datasets: [{
                label: 'Temp (\u00B0F)', data: obs.map(o => o.temp),
                borderColor: '#ef4444', pointRadius: 0, borderWidth: 2, tension: 0.3,
            }, {
                label: 'Dew Point (\u00B0F)', data: obs.map(o => o.dewPoint),
                borderColor: '#65a30d', pointRadius: 0, borderWidth: 2, tension: 0.3,
            }]
        },
        options: commonOpts,
    }));

    // Wind
    historyCharts.push(new Chart(document.getElementById('wind-chart'), {
        type: 'line',
        data: {
            labels,
            datasets: [{
                label: 'Wind (mph)', data: obs.map(o => o.windSpeed),
                borderColor: '#3b82f6', pointRadius: 0, borderWidth: 2, tension: 0.3,
            }, {
                label: 'Gusts (mph)', data: obs.map(o => o.windGust),
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
            labels,
            datasets: [{
                label: 'Precip Rate (in/hr)', data: obs.map(o => o.precipRate),
                borderColor: '#65a30d', backgroundColor: 'rgba(101,163,13,0.2)',
                fill: true, pointRadius: 0, borderWidth: 1, tension: 0.3,
            }, {
                label: 'Precip Total (in)', data: obs.map(o => o.precipTotal),
                borderColor: '#3b82f6', backgroundColor: 'rgba(59,130,246,0.1)',
                fill: true, pointRadius: 0, borderWidth: 2, tension: 0.3,
                yAxisID: 'y1',
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
            labels,
            datasets: [{
                label: 'Pressure (inHg)', data: obs.map(o => o.pressure),
                borderColor: '#8b5cf6', pointRadius: 0, borderWidth: 2, tension: 0.3,
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

// Close popup on Escape key
document.addEventListener('keydown', e => {
    if (e.key === 'Escape') closePopup();
});
