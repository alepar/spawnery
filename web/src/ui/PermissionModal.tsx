export function PermissionModal({ title, onResolve }: { title: string; onResolve: (allow: boolean) => void }) {
  return (
    <div className="modal-backdrop">
      <div className="modal">
        <p>The agent requests permission: <b>{title}</b></p>
        <button onClick={() => onResolve(true)}>Allow</button>
        <button onClick={() => onResolve(false)}>Deny</button>
      </div>
    </div>
  );
}
