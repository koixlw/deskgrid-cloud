"""Doc worker task implementations — text summarization and word frequency."""
import math
import re
import time
from collections import Counter


# --- simple Chinese-friendly sentence splitter ---
_PUNCT = "。！？!?；;\n"
_LATIN_STOP = {"the", "a", "an", "and", "or", "of", "to", "in", "on", "for", "is", "are", "was", "were", "be", "been", "being"}


def split_sentences(text: str) -> list[str]:
    text = re.sub(r"\s+", " ", text).strip()
    if not text:
        return []
    # Split on CJK punctuation, then also on .!? if followed by space + capital
    pattern = r"([。！？!?；;])"
    parts = re.split(pattern, text)
    sents = []
    buf = ""
    for p in parts:
        if p in "。！？!?；;":
            buf += p
            sents.append(buf.strip())
            buf = ""
        else:
            buf += p
    if buf.strip():
        # split on Latin sentence end too
        for piece in re.split(r"(?<=[.!?])\s+(?=[A-Z])", buf):
            if piece.strip():
                sents.append(piece.strip())
    return [s for s in sents if len(s) > 4]


def tokenize(text: str) -> list[str]:
    """Tokenize into a mix of Chinese bigrams and Latin words."""
    tokens: list[str] = []
    # Latin words
    for w in re.findall(r"[A-Za-z][A-Za-z0-9_]+", text):
        lw = w.lower()
        if lw not in _LATIN_STOP and len(lw) > 1:
            tokens.append(lw)
    # Chinese characters: split into characters, then bigrams
    chinese = re.findall(r"[一-鿿]+", text)
    for word in chinese:
        if len(word) == 1:
            tokens.append(word)
        else:
            tokens.extend(word[i:i+2] for i in range(len(word) - 1))
    return tokens


def sentence_similarity(s1: str, s2: str) -> float:
    t1 = set(tokenize(s1))
    t2 = set(tokenize(s2))
    if not t1 or not t2:
        return 0.0
    return len(t1 & t2) / math.sqrt(len(t1) * len(t2))


def textrank(sentences: list[str], top_n: int = 3, damping: float = 0.85, iters: int = 30) -> list[str]:
    """Simplified TextRank: build similarity matrix, run PageRank, pick top_n."""
    n = len(sentences)
    if n == 0:
        return []
    if n <= top_n:
        return sentences

    # similarity matrix
    sim = [[0.0] * n for _ in range(n)]
    for i in range(n):
        for j in range(i + 1, n):
            s = sentence_similarity(sentences[i], sentences[j])
            sim[i][j] = s
            sim[j][i] = s

    # initial score
    score = [1.0] * n
    for _ in range(iters):
        new_score = [0.0] * n
        for i in range(n):
            s = 0.0
            for j in range(n):
                if i == j or sim[j][i] == 0:
                    continue
                denom = sum(sim[j][k] for k in range(n) if k != j) or 1.0
                s += sim[j][i] / denom * score[j]
            new_score[i] = (1 - damping) + damping * s
        if all(abs(new_score[i] - score[i]) < 1e-4 for i in range(n)):
            score = new_score
            break
        score = new_score

    # pick top_n preserving original order
    ranked = sorted(enumerate(score), key=lambda x: -x[1])
    chosen = sorted(ranked[:top_n])
    return [sentences[i] for i, _ in chosen]


def summarize(task_id: str, payload: str, reporter, cancel_event=None) -> dict:
    """Generate a TextRank-style extractive summary."""
    reporter.progress(10, "[summary] 分句")
    time.sleep(0.2)
    sentences = split_sentences(payload)
    if not sentences:
        return {"success": False, "error": "no sentences found"}
    reporter.progress(30, f"[summary] 共 {len(sentences)} 句")
    time.sleep(0.2)

    top_n = max(1, min(5, max(1, len(sentences) // 4)))
    reporter.progress(50, f"[summary] TextRank 抽取 top-{top_n}")
    time.sleep(0.2)
    summary = textrank(sentences, top_n=top_n)

    reporter.progress(90, "[summary] 完成")
    return {
        "success": True,
        "method": "textrank",
        "sentence_count": len(sentences),
        "summary_sentence_count": len(summary),
        "summary": " ".join(summary),
    }


def wordfreq(task_id: str, payload: str, reporter, cancel_event=None) -> dict:
    """Compute top-N word/bigram frequencies."""
    reporter.progress(15, "[wordfreq] 分词")
    time.sleep(0.2)
    tokens = tokenize(payload)
    if not tokens:
        return {"success": False, "error": "no tokens"}
    reporter.progress(50, f"[wordfreq] 共 {len(tokens)} token")
    time.sleep(0.2)
    counter = Counter(tokens)
    top = counter.most_common(20)
    reporter.progress(95, "[wordfreq] 完成")
    return {
        "success": True,
        "total_tokens": len(tokens),
        "unique_tokens": len(counter),
        "top": [{"word": w, "count": c} for w, c in top],
    }
