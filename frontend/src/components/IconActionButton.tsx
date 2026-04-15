import * as Tooltip from "@radix-ui/react-tooltip";
import type { ReactNode } from "react";

interface IconActionButtonProps {
  label: string;
  onClick: () => void;
  children: ReactNode;
  pressed?: boolean;
  tooltipAlign?: "start" | "center" | "end";
  tooltipSide?: "top" | "right" | "bottom" | "left";
}

export function IconActionButton({
  label,
  onClick,
  children,
  pressed,
  tooltipAlign = "center",
  tooltipSide = "bottom",
}: IconActionButtonProps) {
  return (
    <Tooltip.Provider delayDuration={120} skipDelayDuration={80}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <button
            aria-label={label}
            aria-pressed={pressed}
            className="flex h-7 w-7 items-center justify-center text-[#666] outline-none transition hover:text-[#b2b2b2] focus-visible:text-[#b2b2b2]"
            onClick={onClick}
            type="button"
          >
            {children}
          </button>
        </Tooltip.Trigger>

        <Tooltip.Portal>
          <Tooltip.Content
            align={tooltipAlign}
            className="icon-action-tooltip"
            collisionPadding={8}
            side={tooltipSide}
            sideOffset={10}
          >
            {label}
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  );
}
