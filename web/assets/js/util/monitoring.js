/**
 * Shared helpers of the monitoring UI (docs/spec/monitoring-panel.md §7).
 *
 * Three pages read monitoring data — the Monitoring page, the Health column of
 * the inbounds table and the Monitoring settings tab — and they must agree on
 * what a state is worth, how a duration reads and what counts as a probe
 * account. That agreement lives here rather than three times over.
 *
 * Every timestamp is UTC milliseconds as the panel API sends it; the strings
 * come out in the browser's own zone.
 *
 * Pages that use this in a Vue template expose it as `MonUtil` in `data`, since
 * a template can only reach properties of its own instance.
 */
const MonUtil = {
    /**
     * The state a mon-target can be in, worst first. This is the same order
     * the Go side folds by (worstOfStates in web/service): the badge of an
     * inbound is the worst of its live targets.
     */
    RANK: ['DOWN', 'FLAPPING', 'UNKNOWN', 'UP', 'PAUSED'],

    /** STALE is not a target state: the panel paints it over every state it
     *  shows while the mon-server is silent (§7.1). */
    STALE: 'STALE',

    /** Mirrors service.ProbePrefix / IsProbeAccount, case-insensitively. */
    PROBE_PREFIX: 'probe-',

    isProbeAccount(email) {
        return typeof email === 'string' &&
            email.slice(0, MonUtil.PROBE_PREFIX.length).toLowerCase() === MonUtil.PROBE_PREFIX;
    },

    /** The worst of a list of state names, '' when the list is empty. */
    worst(states) {
        let best = -1;
        (states || []).forEach(s => {
            const i = MonUtil.RANK.indexOf(s);
            if (i >= 0 && (best < 0 || i < best)) best = i;
        });
        return best < 0 ? '' : MonUtil.RANK[best];
    },

    /** The CSS class of a state tag; while the panel is stale every tag wears
     *  the STALE style and keeps its real state in the tooltip. */
    stateClass(state, stale) {
        return 'mon-state-' + String(stale ? MonUtil.STALE : (state || 'unknown')).toLowerCase();
    },

    /** "12s ago" / "4m ago" / "3h ago" / "2d ago"; '' for a missing stamp. */
    ago(ms, now) {
        if (!ms) return '';
        return MonUtil.dur(ms, now) + ' ago';
    },

    /** How long ago ms was, unqualified: "12s", "17m", "3h 04m", "2d". */
    dur(ms, now) {
        if (!ms) return '';
        const s = Math.max(0, Math.round(((now || Date.now()) - ms) / 1000));
        if (s < 60) return s + 's';
        const m = Math.floor(s / 60);
        if (m < 60) return m + 'm';
        const h = Math.floor(m / 60);
        if (h < 48) return h + 'h ' + String(m % 60).padStart(2, '0') + 'm';
        return Math.floor(h / 24) + 'd';
    },

    /** "14:58" in the browser's zone. */
    time(ms) {
        return ms ? moment(ms).format('HH:mm') : '';
    },

    /** "14:58 13.09" in the browser's zone. */
    stamp(ms) {
        return ms ? moment(ms).format('HH:mm DD.MM') : '';
    },

    /**
     * uptime24 as the page prints it: "99.8 %". null is not 0 % — it means
     * nothing was measured in the window — so it reads as an em dash.
     */
    uptime(ratio) {
        return (ratio === null || ratio === undefined) ? '—' : (ratio * 100).toFixed(1) + ' %';
    },

    /** latAvg24 as "175 ms"; null means no successful probe carried one. */
    latency(msValue) {
        return (msValue === null || msValue === undefined) ? '—' : Math.round(msValue) + ' ms';
    },

    /** coverage as a whole percentage, for the muted "cov 95 %" note. */
    coverage(ratio) {
        return Math.round((ratio || 0) * 100) + ' %';
    },
};

/**
 * The session endpoints of the Monitoring UI (monitoring-panel.md §7.4 and
 * §7.3), in the panel's {success,msg,obj} envelope.
 *
 * These go through axios rather than HttpUtil because HttpUtil shows every
 * answer as a toast and has no DELETE; the monitoring pages poll every ten
 * seconds and must stay quiet, so each caller decides what to say.
 */
const MonApi = {
    async _call(method, path, data) {
        try {
            const resp = await axios({ method: method, url: '/panel/api/monitoring/' + path, data: data });
            return resp.data || { success: false, msg: '' };
        } catch (e) {
            const body = e.response && e.response.data;
            return { success: false, msg: (body && (body.msg || body.message)) || e.message || 'request failed' };
        }
    },

    targets() {
        return MonApi._call('get', 'targets');
    },
    events(limit) {
        return MonApi._call('get', 'events?limit=' + encodeURIComponent(limit || 50));
    },
    stats(inboundKind, inboundId, range) {
        return MonApi._call('get', 'stats?inboundKind=' + encodeURIComponent(inboundKind) +
            '&inboundId=' + encodeURIComponent(inboundId) + '&range=' + encodeURIComponent(range));
    },
    probe() {
        return MonApi._call('get', 'probe');
    },
    removeProbe() {
        return MonApi._call('delete', 'probe');
    },
    resetToken() {
        return MonApi._call('post', 'token/reset');
    },
};
