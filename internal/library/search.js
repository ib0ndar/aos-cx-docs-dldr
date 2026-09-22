const search = document.querySelector("#search");
const results = document.querySelector("#results");
const status = document.querySelector("#search-status");
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
        count++;
        if (count > 50) continue;
        const article = document.createElement("article");
        article.setAttribute("role", "listitem");
        const link = document.createElement("a");
        link.href = entry.url;
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
