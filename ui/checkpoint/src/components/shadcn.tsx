// Package shadcn provides accessible, composable shadcn/ui React components
// tailored for the sandboxed Surla Architecture Workspace.

import React from "react";

export interface CardProps extends React.HTMLAttributes<HTMLDivElement> {
  children?: React.ReactNode;
}

// Card renders a bordered shadcn/ui surface container.
export function Card({ className = "", children, ...rest }: CardProps) {
  const cls = ["shadcn-card", className].filter(Boolean).join(" ");
  return (
    <div className={cls} {...rest}>
      {children}
    </div>
  );
}

// CardHeader renders the top heading block of a Card.
export function CardHeader({ className = "", children, ...rest }: CardProps) {
  const cls = ["shadcn-card-header", className].filter(Boolean).join(" ");
  return (
    <div className={cls} {...rest}>
      {children}
    </div>
  );
}

// CardTitle renders the primary heading inside a CardHeader.
export function CardTitle({ className = "", children, ...rest }: React.HTMLAttributes<HTMLHeadingElement>) {
  const cls = ["shadcn-card-title", className].filter(Boolean).join(" ");
  return (
    <h3 className={cls} {...rest}>
      {children}
    </h3>
  );
}

// CardDescription renders secondary supporting text inside a CardHeader.
export function CardDescription({ className = "", children, ...rest }: React.HTMLAttributes<HTMLParagraphElement>) {
  const cls = ["shadcn-card-desc", className].filter(Boolean).join(" ");
  return (
    <p className={cls} {...rest}>
      {children}
    </p>
  );
}

// CardContent renders the body slot of a Card.
export function CardContent({ className = "", children, ...rest }: CardProps) {
  return (
    <div className={className} {...rest}>
      {children}
    </div>
  );
}

export interface BadgeProps extends React.HTMLAttributes<HTMLSpanElement> {
  variant?: "default" | "accent" | "success" | "warning" | "destructive";
  children?: React.ReactNode;
}

// Badge renders a compact pill label for status, verification kind, or active policy outcomes.
export function Badge({ variant = "default", className = "", children, ...rest }: BadgeProps) {
  const variantCls =
    variant === "accent"
      ? "shadcn-badge-accent"
      : variant === "success"
      ? "shadcn-badge-success"
      : variant === "warning"
      ? "shadcn-badge-warning"
      : variant === "destructive"
      ? "shadcn-badge-destructive"
      : "";
  const cls = ["shadcn-badge", "surla-badge", variantCls, className].filter(Boolean).join(" ");
  return (
    <span className={cls} {...rest}>
      {children}
    </span>
  );
}

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: "default" | "primary" | "pill";
  children?: React.ReactNode;
}

// Button renders an accessible action button or toggle pill.
export function Button({ variant = "default", className = "", type = "button", children, ...rest }: ButtonProps) {
  const variantCls =
    variant === "primary"
      ? "shadcn-btn-primary surla-btn-primary surla-btn"
      : variant === "pill"
      ? "surla-pill"
      : "surla-btn";
  const cls = ["shadcn-btn", variantCls, className].filter(Boolean).join(" ");
  return (
    <button type={type} className={cls} {...rest}>
      {children}
    </button>
  );
}

export interface AlertProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?: "default" | "warning" | "destructive";
  children?: React.ReactNode;
}

// Alert renders a callout banner for warnings, errors, or preserved drafts.
export function Alert({ variant = "default", className = "", children, ...rest }: AlertProps) {
  const variantCls =
    variant === "destructive"
      ? "shadcn-alert-destructive surla-error"
      : variant === "warning"
      ? "shadcn-alert-warning"
      : "";
  const cls = ["shadcn-alert", variantCls, className].filter(Boolean).join(" ");
  return (
    <div className={cls} {...rest}>
      {children}
    </div>
  );
}

export interface ResponsiveTableProps extends React.TableHTMLAttributes<HTMLTableElement> {
  ariaLabel: string;
  children?: React.ReactNode;
}

// ResponsiveTable wraps a semantic HTML table in a keyboard-scrollable container-query region.
export function ResponsiveTable({ ariaLabel, children, ...rest }: ResponsiveTableProps) {
  return (
    <div className="surla-table-wrap" role="region" aria-label={ariaLabel} tabIndex={0}>
      <table className="shadcn-table" {...rest}>
        {children}
      </table>
    </div>
  );
}
