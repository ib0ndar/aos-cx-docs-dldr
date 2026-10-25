const search = document.querySelector("#search");
const results = document.querySelector("#results");
const status = document.querySelector("#search-status");
const libraryBase = new URL(".", location.href).href;
// Search links come from archived data, so only a relative link that stays
// inside this release directory is used; a scheme such as javascript: is not.
const localLink = value => {
    if (typeof value !== "string" || /^[a-z][a-z\d+.-]*:/i.test(value) || /^[\/\\]/.test(value)) return null;
    try {
        return new URL(value, location.href).href.startsWith(libraryBase) ? value : null;
    } catch {
        return null;
    }
};
search.addEventListener("input", () => {
    results.replaceChildren();
    status.textContent = "";
    const normalize = value => value.normalize("NFKC").toLocaleLowerCase();
    const terms = normalize(search.value).trim().split(/\s+/).filter(Boolean);
    if (!terms.length) return;
    let count = 0;
    for (const entry of window.AOSCX_SEARCH) {
        const haystack = normalize([entry.guide, entry.title, entry.text].join(" "));
        if (!terms.every(term => haystack.includes(term))) continue;
        const href = localLink(entry.url);
        if (!href) continue;
        count++;
        if (count > 50) continue;
        const article = document.createElement("article");
        article.setAttribute("role", "listitem");
        const link = document.createElement("a");
        link.href = href;
        link.textContent = entry.guide + " / " + entry.title;
        article.append(link);
        if (entry.text) {
            const preview = document.createElement("p");
            const normalizedText = normalize(entry.text);
            const at = Math.max(0, normalizedText.indexOf(terms[0]) - 70);
            preview.textContent = entry.text.slice(at, at + 240);
            article.append(preview);
        }
        results.append(article);
    }
    status.textContent = !count ? results.dataset.emptyMessage :
        count > 50 ? "Showing the first 50 matching results." :
        "Showing " + count + (count === 1 ? " matching result." : " matching results.");
});
