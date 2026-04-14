import { CSSProperties, useState, useRef, useEffect } from 'react';
import { Constellation } from '../hooks/useConstellation';

const TAB_LIMIT = 7; // max tabs before overflow dropdown

interface Props {
  constellations: Constellation[];
  active: Constellation;
  onSelect: (id: string) => void;
  loading?: boolean;
}

export default function ConstellationNav({ constellations, active, onSelect, loading }: Props) {
  const [dropdownOpen, setDropdownOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const tabs = constellations.slice(0, TAB_LIMIT);
  const overflow = constellations.slice(TAB_LIMIT);
  const overflowHasActive = overflow.some(c => c.id === active.id);

  // Close dropdown on outside click
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (dropdownRef.current && !dropdownRef.current.contains(e.target as Node)) {
        setDropdownOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClick);
    return () => document.removeEventListener('mousedown', handleClick);
  }, []);

  if (loading && constellations.length <= 1) {
    return <div style={s.bar}><div style={s.loading}>loading constellations...</div></div>;
  }

  return (
    <div style={s.bar}>
      {tabs.map(c => (
        <button
          key={c.id}
          style={{
            ...s.tab,
            ...(c.isSelf ? s.tabSelf : {}),
            ...(active.id === c.id ? s.tabActive : {}),
          }}
          onClick={() => onSelect(c.id)}
          title={c.isSelf ? 'SETI self-monitoring' : c.label}
        >
          {c.isSelf ? (
            <span style={s.selfLabel}>S.E.T.I.</span>
          ) : (
            c.label
          )}
        </button>
      ))}

      {overflow.length > 0 && (
        <div ref={dropdownRef} style={s.overflowWrap}>
          <button
            style={{
              ...s.tab,
              ...(overflowHasActive ? s.tabActive : {}),
            }}
            onClick={() => setDropdownOpen(o => !o)}
          >
            {overflowHasActive ? active.label : `+${overflow.length} more`}
            <span style={s.chevron}>{dropdownOpen ? '▲' : '▼'}</span>
          </button>
          {dropdownOpen && (
            <div style={s.dropdown}>
              {overflow.map(c => (
                <button
                  key={c.id}
                  style={{
                    ...s.dropdownItem,
                    ...(active.id === c.id ? s.dropdownItemActive : {}),
                  }}
                  onClick={() => { onSelect(c.id); setDropdownOpen(false); }}
                >
                  {c.label}
                </button>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

const s: Record<string, CSSProperties> = {
  bar: {
    display: 'flex',
    alignItems: 'stretch',
    gap: 0,
    background: '#0d1117',
    borderBottom: '1px solid #21262d',
    padding: '0 20px',
    overflowX: 'auto',
  },
  loading: {
    fontSize: 10,
    color: '#484f58',
    letterSpacing: 1,
    padding: '10px 0',
    alignSelf: 'center',
  },
  tab: {
    background: 'none',
    border: 'none',
    borderBottom: '2px solid transparent',
    color: '#6e7681',
    fontSize: 11,
    letterSpacing: 1,
    cursor: 'pointer',
    padding: '10px 16px',
    fontFamily: 'inherit',
    whiteSpace: 'nowrap',
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    transition: 'color 0.1s, border-color 0.1s',
  },
  tabActive: {
    color: '#e6edf3',
    borderBottomColor: '#58a6ff',
  },
  tabSelf: {
    borderRight: '1px solid #21262d',
    marginRight: 4,
    paddingRight: 20,
  },
  selfLabel: {
    fontSize: 11,
    fontWeight: 700,
    color: 'inherit',
    letterSpacing: 2,
  },
  chevron: {
    fontSize: 8,
    opacity: 0.6,
  },
  overflowWrap: {
    position: 'relative',
  },
  dropdown: {
    position: 'absolute',
    top: '100%',
    left: 0,
    background: '#161b22',
    border: '1px solid #21262d',
    borderRadius: 6,
    zIndex: 100,
    minWidth: 160,
    boxShadow: '0 4px 12px rgba(0,0,0,0.4)',
    overflow: 'hidden',
  },
  dropdownItem: {
    display: 'block',
    width: '100%',
    background: 'none',
    border: 'none',
    borderBottom: '1px solid #21262d',
    color: '#8b949e',
    fontSize: 12,
    cursor: 'pointer',
    padding: '10px 16px',
    fontFamily: 'inherit',
    textAlign: 'left',
  },
  dropdownItemActive: {
    color: '#e6edf3',
    background: 'rgba(88,166,255,0.08)',
  },
};
