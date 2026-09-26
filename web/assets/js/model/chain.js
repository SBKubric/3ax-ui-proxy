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
    /** One health badge per hop: {name, role, state} (§6.4). */
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
    /**
     * Deletes a hop. skipDrain drops the row at once instead of letting the
     * hop hand its neighbours over (§4.5.9) — the runbook's flag for a box
     * that is already dead. The answer says which of the two happened.
     */
    del(id, force, skipDrain) {
        return HttpUtil.postJson(ChainApi._base + 'del/' + encodeURIComponent(id),
            { force: !!force, skipDrain: !!skipDrain });
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
    STATE_DRAINING: 'draining',

    ROLE_INNER: 'inner',
    ROLE_EDGE: 'edge',

    /** The badge of a hop with no monitoring data (pending, draining, new). */
    HEALTH_NONE: 'NONE',

    /** The tag colour of a state; legacy is deliberately colourless. */
    stateColor(state) {
        return { pending: 'orange', joined: 'green', legacy: 'default', draining: 'red' }[state] || 'default';
    },

    /**
     * The health badge's class, reusing the Monitoring page's styles
     * (§7.4): mon-state-up / -down / … / -stale, and mon-state-none for a
     * hop without data.
     */
    healthClass(health) {
        return 'mon-state mon-state-' + String(health || ChainUtil.HEALTH_NONE).toLowerCase();
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

    /**
     * A hop on its way out (§4.5). It is in the registry but not in the chain:
     * it serves its former neighbours until they have re-chained, and nothing
     * about it can be changed any more — which is why its row carries no
     * buttons.
     */
    isDraining(hop) {
        return !!hop && hop.state === ChainUtil.STATE_DRAINING;
    },

    /**
     * The one server name the chain-following inbounds accept while this edge
     * is active: the one given, or else the host of the neighbour target —
     * the registry's own rule (ChainHop.NeighbourServerName).
     */
    neighbourServerName(hop) {
        if (!hop || !hop.realityTarget) return '';
        if (hop.realityServerName) return hop.realityServerName;
        const target = hop.realityTarget;
        const colon = target.lastIndexOf(':');
        const host = colon > 0 ? target.slice(0, colon) : target;
        return host.replace(/^\[|\]$/g, '');
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
