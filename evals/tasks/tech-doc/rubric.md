# Rubric — REST vs gRPC comparison document

Score 0-10 on how well comparison.md meets the brief. Weigh:

1. **Coverage** — addresses all requested points: an intro framing the decision,
   how each works (protocol, payload, contract/schema), tradeoffs across
   performance, streaming, browser/client support, tooling, and debuggability,
   plus "use REST when…" / "use gRPC when…" guidance and a summary recommendation.
2. **Technical accuracy** — claims are correct and specific (e.g. gRPC over
   HTTP/2 with protobuf and code-gen stubs; REST/JSON's human-readability and
   ubiquitous browser support; gRPC-Web's proxy requirement). No factual errors
   or hand-waving.
3. **Structure & usefulness** — proper Markdown: clear headings, lists, and at
   least one comparison table. Reads like real decision-support, not filler.
4. **Depth** — goes beyond surface bullet points; the tradeoff discussion shows
   genuine understanding and would actually help someone choose.

A vague, generic, or inaccurate doc scores ≤5. An accurate, well-structured,
genuinely helpful comparison scores 8+.
