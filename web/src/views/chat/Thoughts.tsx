import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

export function Thoughts({ text }: { text: string }) {
  if (!text) return null;
  return (
    <div className="mx-auto max-w-[70ch] px-4 py-1" data-role="thought">
      <Collapsible>
        <CollapsibleTrigger className="text-xs text-muted-foreground hover:text-foreground">
          ▸ thinking
        </CollapsibleTrigger>
        <CollapsibleContent>
          <pre className="mt-1 whitespace-pre-wrap rounded-md bg-muted p-2 text-xs text-muted-foreground">
            {text}
          </pre>
        </CollapsibleContent>
      </Collapsible>
    </div>
  );
}
