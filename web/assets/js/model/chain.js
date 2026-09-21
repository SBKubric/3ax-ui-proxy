/**
 * The chain registry as the panel's own pages read it
 * (docs/spec/proxy-chain.md §2.4, §7).
 *
 * ChainApi is the one place that knows the route names; the editor calls
 * methods, never URLs. Every answer is the panel's {success, msg, obj}
 * envelope, and HttpUtil already turns a refusal into a toast carrying the
 * localized wording the controller picked, so callers only have to decide what
 * to do, not what to say.
 *
 * Writes go through postJson, not post: the panel's axios sends a form body by
 * default (assets/js/axios-init.js), and these routes read JSON, which is what
 * §2.4 documents and what the bot and a future orchestrator will send too.
 *
 * ChainUtil holds the few derivations both the editor and the read-only
 * override row make from a hop: whether it is fresh, how its token is shown,
 * how a stamp reads. A Vue template can only reach properties of its own
 * instance, so a page that uses these in markup exposes them in `data`.
 */
const ChainApi = {
    _base: '/panel/api/chain/',

    /** The whole registry: revision, active edge, hops, ports banner. */
    list() {
        return HttpUtil.get(ChainApi._base + 'list');
    },
    /** One health badge per hop; UNKNOWN for all of them until #87 lands. */
    health() {
        return HttpUtil.get(ChainApi._base + 'hops/health');
    },
    /** The relayed ports of the chain document — `x-ui chain ports`' list. */
    ports() {
        return HttpUtil.get(ChainApi._base + 'ports');
    },
    /** Creates a hop; the answer carries its join token, once and never again. */
    add(hop) {
        return HttpUtil.postJson(ChainApi._base + 'add', hop);
    },
    update(id, patch) {
        return HttpUtil.postJson(ChainApi._base + 'update/' + encodeURIComponent(id), patch);
    },
    del(id, force) {
        return HttpUtil.postJson(ChainApi._base + 'del/' + encodeURIComponent(id), { force: !!force });
    },
    setActive(id) {
        return HttpUtil.postJson(ChainApi._base + "setActive/" + encodeURIComponent(id), {});
    },
    /** The panel's own "/proxy off": no edge stays active. */
    clearActive() {
        return HttpUtil.postJson(ChainApi._base + "clearActive", {});
    },
    reissueToken(id) {
        return HttpUtil.postJson(ChainApi._base + "reissueToken/" + encodeURIComponent(id), {});
    },
};

const ChainUtil = {
    STATE_PENDING: 'pending',
    STATE_JOINED: 'joined',
    STATE_LEGACY: 'legacy',

    ROLE_INNER: 'inner',
    ROLE_EDGE: 'edge',

    /** The tag colour of a state; legacy is deliberately colourless. */
    stateColor(state) {
        return { pending: 'orange', joined: 'green', legacy: 'default' }[state] || 'default';
    },

    /**
     * The health badge's class, reusing the Monitoring page's styles
     * (§7.4): mon-state-up / -down / -unknown.
     */
    healthClass(health) {
        return 'mon-state mon-state-' + String(health || 'unknown').toLowerCase();
    },

    /** "2026-09-21 12:00 UTC" — the one format a join token's expiry reads in. */
    stamp(ms) {
        if (!ms) return '';
        return new Date(ms).toISOString().slice(0, 16).replace('T', ' ') + ' UTC';
    },

    /**
     * A join token as the editor shows it after it was issued. The panel keeps
     * only the hash, so this is the single moment the clear token exists in a
     * browser: it is shown whole, because a masked token cannot be copied and
     * copying it is the entire point.
     */
    tokenText(token) {
        return token || '';
    },

    /** Only a pending hop has a join token to show at all (§4.1). */
    hasToken(hop) {
        return !!hop && hop.state === ChainUtil.STATE_PENDING;
    },

    /** A legacy hop never entered by token: it wants re-installing (§2.3). */
    isLegacy(hop) {
        return !!hop && hop.state === ChainUtil.STATE_LEGACY;
    },

    /**
     * Whether a hop has confirmed the current revision. Freshness is not
     * health: it answers "has the configuration reached this box", while the
     * badge answers "does traffic go through it" (§7.4).
     */
    isCurrent(hop, revision) {
        return !!hop && Number(hop.lastRevision || 0) >= Number(revision || 0);
    },
};
