import type { ReactNode } from "react";

interface IconActionButtonProps {
  label: string;
  onClick: () => void;
  children: ReactNode;
  pressed?: boolean;
}

export function IconActionButton({
  label,
  onClick,
  children,
  pressed,
}: IconActionButtonProps) {
  return (
    <button
      aria-label={label}
      aria-pressed={pressed}
      className="flex h-7 w-7 items-center justify-center text-[#666] outline-none transition hover:text-[#b2b2b2] focus-visible:text-[#b2b2b2]"
      onClick={onClick}
      type="button"
    >
      {children}
    </button>
  );
}
